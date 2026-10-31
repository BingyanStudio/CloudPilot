#!/usr/bin/env node
import fs from 'node:fs';
import readline from 'node:readline';

if (process.argv[2] !== 'app-server') throw new Error('fixture expects app-server');
const stateFile = '/workspace/.codex/fixture-thread.json';
let state;
let active;
let completion;
const write = value => console.log(JSON.stringify(value));
const response = (id, result) => write({jsonrpc:'2.0', id, result});
const error = (id, message) => write({jsonrpc:'2.0', id, error:{code:-32000,message}});
const notification = (method, params) => write({jsonrpc:'2.0', method, params});
const save = () => fs.writeFileSync(stateFile, JSON.stringify(state));
const textInput = input => (input || []).map(item => item.text || '').join('\n');
function complete(status = 'completed') {
  if (!active) return;
  clearTimeout(completion);
  if (status === 'completed') {
    const text = `Fixture turn ${state.count} completed; workspace counter ${fs.readFileSync('/workspace/fixture-counter.txt','utf8')}; messages: ${state.messages.join(' | ')}`;
    const item = {type:'agentMessage',id:`answer-${active}`,text};
    notification('item/completed',{threadId:state.id,turnId:active,item});
    fs.writeFileSync('/output/fixture.txt', `turn ${state.count}`);
    fs.writeFileSync('/output/artifacts.json', JSON.stringify({artifacts:[{kind:'file',name:'Turn output',path:'fixture.txt'}]}));
  }
  save();
  notification('turn/completed',{threadId:state.id,turn:{id:active,status,error:null}});
  active = null;
}
const lines = readline.createInterface({input:process.stdin});
lines.on('line', line => {
  const request = JSON.parse(line);
  const params = request.params || {};
  switch (request.method) {
  case 'initialize':response(request.id,{userAgent:'cloudpilot-session-fixture'});break;
  case 'initialized':break;
  case 'thread/start':
    state = {id:'fixture-thread-'+Date.now(),count:0,messages:[]};save();
    response(request.id,{thread:{id:state.id}});break;
  case 'thread/resume':
    state=JSON.parse(fs.readFileSync(stateFile,'utf8'));
    if (state.id !== params.threadId) {error(request.id,'thread not found');break}
    response(request.id,{thread:{id:state.id}});break;
  case 'turn/start': {
    const text=textInput(params.input);
    state.messages.push(text);state.count++;active=`turn-${state.count}`;
    fs.writeFileSync('/workspace/fixture-counter.txt',String(state.count));save();
    response(request.id,{turn:{id:active,status:'inProgress'}});
    notification('turn/started',{threadId:state.id,turn:{id:active,status:'inProgress'}});
    notification('item/started',{threadId:state.id,turnId:active,item:{type:'commandExecution',id:`tool-${active}`,command:'node increment-workspace-counter',status:'inProgress'}});
    notification('item/commandExecution/outputDelta',{threadId:state.id,turnId:active,itemId:`tool-${active}`,delta:`counter=${state.count}\n`});
    notification('item/completed',{threadId:state.id,turnId:active,item:{type:'commandExecution',id:`tool-${active}`,command:'node increment-workspace-counter',aggregatedOutput:`counter=${state.count}\n`,exitCode:0,status:'completed'}});
    const secret=process.env.OPENAI_API_KEY;
    const split=Math.floor(secret.length/2);
    notification('item/agentMessage/delta',{threadId:state.id,turnId:active,itemId:`stream-${active}`,delta:'Working with '+secret.slice(0,split)});
    notification('item/agentMessage/delta',{threadId:state.id,turnId:active,itemId:`stream-${active}`,delta:secret.slice(split)+'...'});
    notification('item/completed',{threadId:state.id,turnId:active,item:{type:'agentMessage',id:`stream-${active}`,text:'Working with '+secret+'...'}});
    if (!/\b(wait|hold)\b/i.test(text)) completion=setTimeout(()=>complete(),250);
    break;
  }
  case 'turn/steer':
    if (!active || params.expectedTurnId!==active){error(request.id,'turn is no longer active');break}
    state.messages.push(textInput(params.input));save();response(request.id,{turnId:active});
    notification('item/completed',{threadId:state.id,turnId:active,item:{type:'agentMessage',id:'steer-'+Date.now(),text:'Supplement received: '+textInput(params.input)}});
    break;
  case 'turn/interrupt':
    if (!active || params.turnId!==active){error(request.id,'turn is no longer active');break}
    response(request.id,{});complete('interrupted');break;
  default:if(request.id!==undefined)error(request.id,'unsupported method '+request.method);
  }
});
lines.on('close',()=>{clearTimeout(completion);process.exit(0)});
