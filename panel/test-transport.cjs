const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const {webcrypto}=require('node:crypto');
const source=fs.readFileSync(process.argv[2],'utf8');
const updateSandbox={};
vm.createContext(updateSandbox);
vm.runInContext(source+'\nglobalThis.updateTest={readUpdateStatus,updateMessage};',updateSandbox);
const {readUpdateStatus,updateMessage}=updateSandbox.updateTest;
const attempt='0123456789abcdef0123456789abcdef';
const unrelated='ffffffffffffffffffffffffffffffff';
assert.equal(readUpdateStatus(`APU1 ${attempt} install_failed\nAPU1 ${unrelated} current\nAPU1 ${attempt} updat`,attempt),'install_failed');
assert.equal(readUpdateStatus(`APU1 ${unrelated} current\n`,attempt),null);
assert.equal(readUpdateStatus(`APU1 ${attempt} current\n`,attempt),'current');
assert.equal(readUpdateStatus(`APU1 ${attempt} install_failed_restore_failed\n`,attempt),'install_failed_restore_failed');
assert.match(updateMessage('install_failed_restore_failed'),/YAM.*Luna/);
let running=0, log='', executions=0, epoch='test-epoch';
const records=new Map();
function reply(request){
  if(records.has(request.id))return records.get(request.id);
  const result={id:request.id,epoch,status:200,body:{saved:true}};
  if(request.epoch && request.epoch!==epoch){
    result.status=409;result.body={error:'restarted'};
  } else {executions++;}
  records.set(request.id,result);return result;
}
async function fetch(url,options={}){
  if(url.endsWith('/invoke')){
    const invocation=JSON.parse(JSON.parse(options.body).args[0]);
    const request=invocation.request;
    const result={...reply(request),attempt:invocation.attempt},mine=++running;
    // Like Luna: starting another invocation kills this helper but not
    // the already committed receiver operation.
    setTimeout(()=>{
      if(mine===running)log+='\nAP1 '+Buffer.from(JSON.stringify(result)).toString('base64')+'\n';
    },25);
    return {ok:true,status:202};
  }
  return {ok:true,status:200,text:async()=>log};
}
function tab(){
  const sandbox={crypto:webcrypto,fetch,TextDecoder,Uint8Array,atob,
    performance,setTimeout,Map,Math,Promise,JSON,Error,console};
  vm.createContext(sandbox);
  vm.runInContext(source+'\nglobalThis.panelAPI=api;',sandbox);
  return sandbox.panelAPI;
}
(async()=>{
  const a=tab(),b=tab();
  const results=await Promise.all([a('status'),b('status')]);
  assert(results.every(x=>x.saved));
  assert.equal(executions,2,'cancelled invocation must not execute twice');
  // Same-tab overlapping reads are coalesced, not queued indefinitely.
  const count=executions;
  await Promise.all([a('status'),a('status'),a('status')]);
  assert.equal(executions,count+1);
  log+='\nAP1 partial-cancelled-output\n';
  await a('settings');
  epoch='new-epoch';
  await assert.rejects(a('settings','PUT',{}),/перезапущен/);
  console.log('TRANSPORT_RETRY_MULTITAB_COALESCING_EPOCH_PASS');
})().catch(e=>{console.error(e);process.exitCode=1;});
