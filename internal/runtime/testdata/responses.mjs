import http from 'node:http';
let counter = 0;
http.createServer(async (request, response) => {
  if (request.url === '/health') { response.end('ok'); return; }
  let body = '';
  for await (const chunk of request) body += chunk;
  let data;
  try { data = JSON.parse(body); } catch { response.writeHead(400).end('invalid body'); return; }
  if (request.url !== '/v1/responses' || request.headers.authorization !== 'Bearer fixture-protocol-key' || data.model !== 'gpt-5.4') {
    response.writeHead(400).end('unexpected endpoint, model, or authentication'); return;
  }
  const text = JSON.stringify(data.input).includes('interactive follow-up') ? 'Verified resumed Codex conversation.' : 'Verified local Codex protocol.';
  counter++;
  const item = {id:'msg_fixture_'+counter, type:'message', status:'completed', role:'assistant', content:[{type:'output_text', text, annotations:[]}]};
  const result = {id:'resp_fixture_'+counter, object:'response', created_at:1, status:'completed', output:[item], usage:{input_tokens:5, output_tokens:5, total_tokens:10}};
  const events = [
    {type:'response.created', response:{...result, status:'in_progress', output:[]}},
    {type:'response.output_item.added', output_index:0, item:{...item, status:'in_progress', content:[]}},
    {type:'response.content_part.added', item_id:item.id, output_index:0, content_index:0, part:{type:'output_text', text:'', annotations:[]}},
    {type:'response.output_text.delta', item_id:item.id, output_index:0, content_index:0, delta:text},
    {type:'response.output_text.done', item_id:item.id, output_index:0, content_index:0, text},
    {type:'response.content_part.done', item_id:item.id, output_index:0, content_index:0, part:item.content[0]},
    {type:'response.output_item.done', output_index:0, item},
    {type:'response.completed', response:result},
  ];
  response.writeHead(200, {'Content-Type':'text/event-stream'});
  for (const event of events) response.write(`data: ${JSON.stringify(event)}\n\n`);
  response.end();
}).listen(8080, '0.0.0.0');
