// Same-origin transport for official Luna 0.2.26. No browser runtime tokens.
let rpcEpoch='',rpcQueue=Promise.resolve();
const pendingReads=new Map();
function requestID(){
  return Array.from(crypto.getRandomValues(new Uint8Array(16)),x=>x.toString(16).padStart(2,'0')).join('');
}
async function invokePanel(request){
  // Retries keep EXACTLY the same ID, epoch and payload. The receiver caches
  // the outcome independently of the helper process cancelled by another tab.
  const deadline=performance.now()+12000;
  let lastInvoke=-Infinity,attempt='';
  while(performance.now()<deadline){
    if(performance.now()-lastInvoke>1600+Math.random()*400){
      attempt=requestID();
      const response=await fetch('/api/plugins/airplay.panel-command/invoke',{
        method:'POST',headers:{'Content-Type':'application/json','X-Luna-Local':'1'},
        body:JSON.stringify({args:[JSON.stringify({request,attempt})]})
      });
      if(!response.ok)throw Error('Включите AirPlay Panel Command в Luna: HTTP '+response.status);
      lastInvoke=performance.now();
    }
    const response=await fetch('/api/plugins/airplay.panel-command/logs',{cache:'no-store'});
    if(!response.ok)throw Error('Не удалось получить ответ helper');
    const lines=(await response.text()).split('\n');
    for(let i=lines.length-1;i>=0;i--){
      if(!lines[i].startsWith('AP1 '))continue;
      let result;
      try{
        const bytes=Uint8Array.from(atob(lines[i].slice(4)),c=>c.charCodeAt(0));
        result=JSON.parse(new TextDecoder().decode(bytes));
      }catch{continue;} // Partial output from a cancelled helper is not a reply.
      if(result.id!==request.id||result.attempt!==attempt)continue;
      if(result.status===503)continue;
      if(rpcEpoch && result.epoch!==rpcEpoch)throw Error('AirPlay перезапущен. Обновите страницу.');
      rpcEpoch=result.epoch;
      if(result.status<200||result.status>=300){
        const error=new Error(result.body?.error||'HTTP '+result.status);
        error.status=result.status;
        throw error;
      }
      return result.body;
    }
    await new Promise(resolve=>setTimeout(resolve,150+Math.random()*150));
  }
  throw Error('Ответ не получен. Операция могла выполниться; обновите настройки перед повтором.');
}
function api(path,method='GET',body){
  const key=method==='GET'?path:null;
  if(key&&pendingReads.has(key))return pendingReads.get(key);
  const task=rpcQueue.catch(()=>{}).then(()=>invokePanel({
    id:requestID(),epoch:rpcEpoch,operation:path,method,
    ...(body===undefined?{}:{body})
  }));
  rpcQueue=task;
  if(key){
    pendingReads.set(key,task);
    task.finally(()=>{if(pendingReads.get(key)===task)pendingReads.delete(key);}).catch(()=>{});
  }
  return task;
}

const updateMessages={
  current:'Уже установлена версия из реестра.',
  updated:'Обновление установлено. Luna приняла команды запуска.',
  plan_failed:'Не удалось проверить версию в реестре. AirPlay не остановлен.',
  busy:'Другое обновление AirPlay уже выполняется.',
  launch_failed:'Не удалось запустить обновление.',
  stop_failed:'Не удалось остановить AirPlay. Установка не началась.',
  install_failed:'YAM не установил обновление.',
  verify_failed:'Не удалось подтвердить установленную версию.',
  restore_failed:'Обновление установлено, но Luna не подтвердила запуск.',
  interrupted:'Обновление прервано.'
};
function readUpdateStatus(text,attempt){
  const prefix='APU1 '+attempt+' ';
  const lines=text.split('\n').slice(0,-1);
  for(let i=lines.length-1;i>=0;i--){
    if(!lines[i].startsWith(prefix))continue;
    const status=lines[i].slice(prefix.length).trim();
    if(Object.hasOwn(updateMessages,status)||status==='pending')return status;
    if(status.endsWith('_restore_failed')&&Object.hasOwn(updateMessages,status.slice(0,-15)))return status;
  }
  return null;
}
function updateMessage(status){
  return updateMessages[status]||updateMessages[status.slice(0,-15)]+' Luna не подтвердила повторный запуск.';
}
async function invokeUpdate(request){
  return fetch('/api/plugins/airplay.panel-command/invoke',{
    method:'POST',headers:{'Content-Type':'application/json','X-Luna-Local':'1'},
    body:JSON.stringify({args:[JSON.stringify(request)]}),signal:AbortSignal.timeout(8000)
  });
}
async function runPackageUpdate(progress){
  const attempt=requestID();
  const started=performance.now();
  const start=await invokeUpdate({update:true,attempt});
  if(!start.ok)throw Error('Не удалось запустить обновление: HTTP '+start.status);
  let nextRead=started+5000;
  while(performance.now()-started<420000){
    await new Promise(resolve=>setTimeout(resolve,1000));
    try{
      const response=await fetch('/api/plugins/airplay.panel-command/logs',{cache:'no-store',signal:AbortSignal.timeout(8000)});
      if(response.ok){
        const status=readUpdateStatus(await response.text(),attempt);
        if(status&&status!=='pending')return status;
      }
      if(performance.now()>=nextRead){
        nextRead=performance.now()+5000;
        await invokeUpdate({update_status:true,attempt});
      }
    }catch{} // Luna can temporarily reject requests while switching plugins.
    progress(Math.round((performance.now()-started)/1000));
  }
  throw Error('Результат обновления не получен. Проверьте AirPlay в Luna перед повтором.');
}
