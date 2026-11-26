const assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
const make=vm.runInNewContext(fs.readFileSync(process.argv[2],'utf8')+'\ncreateAutosave');
(async()=>{
  let scheduled, resolve, reject;
  const writes=[], states=[];
  const save=make({schedule:fn=>scheduled=fn,cancel:()=>{scheduled=null;},
    write:value=>{writes.push(value);return new Promise((yes,no)=>{resolve=yes;reject=no;});},
    notify:(...args)=>states.push(args)});
  save.edit(100);save.edit(70);
  const first=scheduled();
  assert.deepEqual(writes,[70]);
  save.edit(50);await scheduled();
  assert.deepEqual(writes,[70]);
  resolve({saved:true,restart_required:true});await first;
  const second=scheduled();
  save.edit(20);reject(Error('offline'));await second;
  assert.equal(states[states.length-1][0],'error');
  assert.equal(scheduled,null);
  assert.equal(save.unsettled(),true);
  const retry=save.retry();
  assert.deepEqual(writes,[70,50,20]);
  resolve({saved:true});await retry;
  assert.equal(states[states.length-1][0],'saved');
  assert.equal(states[states.length-1][1],true);
  assert.equal(save.unsettled(),false);
  console.log('AUTOSAVE_DEBOUNCE_SERIALIZATION_RETRY_PASS');
})().catch(error=>{console.error(error);process.exitCode=1;});
