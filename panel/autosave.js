// One write at a time. Edits made during a write are saved after that write,
// and an ambiguous transport error never produces an automatic retry loop.
function createAutosave({write, notify, delay=650, schedule=setTimeout, cancel=clearTimeout}){
  let timer, pending=null, active=false, failed=false, restart=false;
  function arm(){
    cancel(timer);
    timer=schedule(flush,delay);
  }
  async function flush(){
    cancel(timer);
    if(active||failed||!pending)return;
    const next=pending;
    pending=null;
    active=true;
    notify('saving');
    try{
      const result=await write(next);
      if(!result?.saved)throw Error('Приёмник не подтвердил сохранение');
      restart=restart||!!result.restart_required;
      notify(pending?'pending':'saved',restart);
    }catch(error){
      cancel(timer);
      pending=pending||next;
      failed=true;
      notify('error',restart,error);
    }finally{
      active=false;
      if(pending&&!failed)arm();
    }
  }
  return {
    edit(next){pending=next;if(!failed){notify('pending',restart);arm();}},
    retry(){failed=false;return flush();},
    unsettled(){return active||!!pending;}
  };
}
