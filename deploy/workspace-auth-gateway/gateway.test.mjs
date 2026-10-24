import test from 'node:test';
import assert from 'node:assert/strict';
import { createGateway } from './gateway.mjs';
import http from 'node:http';

const token = 'fixture-workspace-token-not-a-real-key';
const origin = 'http://127.0.0.1:18787';
const upstreams = Object.fromEntries(['bridge','qmd','dsh','cpa','newapi'].map((n,i)=>[n,`http://127.0.0.1:${21000+i}`]));
async function fixture(t, extra = {}) {
  const calls = [];
  const server = createGateway({ token, origin, upstreams, testMode: true,
    fetcher: async (url, options) => {
      calls.push({ url, ...options });
      if (url.endsWith('/api/user/login/encryption-key')) return Response.json({ success: true, data: { enabled: false } });
      if (url.endsWith('/api/user/login')) return Response.json({ success: true, data: { user: { id: 9 }, access_token: 'fixture-admin-session', token_type: 'bearer' } });
      if (url.includes('/api/channel/')) return Response.json({ success: true, data: { items: [{ id: 1, key: 'must-not-be-returned', api_key: 'must-not-leak-api-key', access_token: 'must-not-leak-token', client_secret: 'must-not-leak-secret', label: 'visible' }] } });
      return Response.json({ ok: true });
    }, ...extra });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  t.after(()=>{server.closeAllConnections();return new Promise(resolve=>server.close(resolve));});
  const base = `http://127.0.0.1:${server.address().port}`;
  const request = (path, options={}) => new Promise((resolve,reject)=>{
    const req=http.request(base+path,{method:options.method||'GET',headers:{host:new URL(origin).host,...options.headers}},res=>{
      const parts=[];res.on('data',p=>parts.push(p));res.on('end',()=>resolve(new Response(Buffer.concat(parts),{status:res.statusCode,headers:res.headers})));
    });req.on('error',reject);req.end(options.body);
  });
  const login = async () => {
    const response = await request('/workspace/login',{method:'POST',headers:{origin,'content-type':'application/json'},body:JSON.stringify({token})});
    assert.equal(response.status,200);
    const cookie = response.headers.get('set-cookie').split(';')[0];
    const { csrf } = await response.json(); return { cookie, csrf };
  };
  return { calls, request, login, base };
}
const authorized = { authorization: 'Bearer '+token };
test('requires token, fixed hosts, and TLS outside explicit test mode',()=>{
  assert.throws(()=>createGateway({token:'',origin,upstreams,testMode:true}));
  assert.throws(()=>createGateway({token,origin,upstreams}));
  assert.throws(()=>createGateway({token,origin,upstreams:{...upstreams,cpa:'http://evil.example'},testMode:true}));
});
test('public live and UI; services reject missing/wrong credentials without upstream work',async t=>{
  const f=await fixture(t);
  assert.equal((await f.request('/live')).status,200);
  assert.equal((await f.request('/')).status,200);
  for(const headers of [{},{authorization:'Bearer invalid'},{authorization:'Bearer '+token+', Bearer '+token}])
    assert.equal((await f.request('/bridge/api/v1/namespaces',{headers})).status,401);
  assert.equal(f.calls.length,0);
});
test('same token reaches each fixed service; original cookies and native headers never pass through',async t=>{
  const f=await fixture(t);
  for(const path of ['/bridge/api/v1/namespaces','/qmd/mcp','/dsh/health','/cpa/v1/models']){
    const response=await f.request(path,{headers:{...authorized,cookie:'private=never-forward','new-api-user':'666'}});
    assert.equal(response.status,200);
    const call=f.calls.at(-1);
    assert.equal(call.headers.authorization,'Bearer '+token);assert.equal(call.headers.cookie,undefined);assert.equal(call.headers['new-api-user'],undefined);
  }
});
test('browser login creates HttpOnly same-site cookie and CSRF; logout invalidates it',async t=>{
  const f=await fixture(t); const {cookie,csrf}=await f.login();
  assert.equal((await f.request('/workspace/session',{headers:{cookie}})).status,200);
  assert.equal((await f.request('/workspace/logout',{method:'POST',headers:{cookie,origin}})).status,403);
  assert.equal((await f.request('/workspace/logout',{method:'POST',headers:{cookie,origin,'x-workspace-csrf':csrf}})).status,200);
  assert.equal((await f.request('/workspace/session',{headers:{cookie}})).status,401);
});
test('expired sessions fail closed and do not fall back after bad explicit bearer',async t=>{
  let now=100; const f=await fixture(t,{now:()=>now,ttlMs:1000}); const {cookie}=await f.login();
  assert.equal((await f.request('/workspace/session',{headers:{cookie,authorization:'Bearer invalid'}})).status,401);
  now=1101;assert.equal((await f.request('/workspace/session',{headers:{cookie}})).status,401);
});
test('rejects hostile host, origin, path escape, login CSRF, and arbitrary proxy',async t=>{
  const f=await fixture(t);
  assert.equal((await f.request('/live',{headers:{host:'evil.example'}})).status,421);
  assert.equal((await f.request('/bridge/api/v1/namespaces',{headers:{...authorized,origin:'https://evil.example'}})).status,403);
  assert.equal((await f.request('/bridge/%252e%252e/x',{headers:authorized})).status,400);
  assert.equal((await f.request('/proxy?url=http://evil.example',{headers:authorized})).status,404);
  assert.equal((await f.request('/workspace/login',{method:'POST',body:JSON.stringify({token})})).status,403);
  assert.equal(f.calls.length,0);
});
test('New API model credentials are distinct from administrator login',async t=>{
  const f=await fixture(t,{newapi:{username:'fixture',password:'fixture-admin-password',modelKey:'fixture-model-key'}});
  assert.equal((await f.request('/newapi/v1/models',{headers:authorized})).status,200);
  assert.equal(f.calls.length,1);assert.equal(f.calls[0].headers.authorization,'Bearer fixture-model-key');
  const admin=await f.request('/newapi/api/channel/',{headers:authorized});assert.equal(admin.status,200);
  const call=f.calls.at(-1);assert.equal(call.headers.authorization,'Bearer fixture-admin-session');assert.equal(call.headers['new-api-user'],'9');
  const adminBody=(await admin.json()).data.items[0];
  for(const field of ['key','api_key','access_token','client_secret'])assert.equal(adminBody[field],'[redacted]');
  assert.equal(adminBody.label,'visible');
  assert.equal(f.calls.filter(c=>c.url.endsWith('/api/user/login')).length,1);
});
test('missing model credential never triggers admin login or fake catalog',async t=>{
  const f=await fixture(t,{newapi:{username:'fixture',password:'fixture-password'}});
  assert.equal((await f.request('/newapi/v1/models',{headers:authorized})).status,503);
  assert.equal(f.calls.length,0);
});
test('mutating request forwarded once; no automatic retry on rejected administrator session',async t=>{
  let writes=0;
  const f=await fixture(t,{newapi:{username:'fixture',password:'fixture-password'},fetcher:async(url)=>{
    if(url.endsWith('/encryption-key'))return Response.json({data:{enabled:false}});
    if(url.endsWith('/login'))return Response.json({success:true,data:{user:{id:1},access_token:'fixture-jwt',token_type:'bearer'}});
    writes++;return Response.json({error:'never expose details'},{status:401});
  }});
  const response=await f.request('/newapi/api/channel/1/status',{method:'POST',headers:{...authorized,'content-type':'application/json'},body:'{"status":2}'});
  assert.equal(response.status,401);assert.equal(writes,1);assert.doesNotMatch(await response.text(),/never expose details/);
});
test('upstream redirects and credential responses are never forwarded',async t=>{
  const f=await fixture(t,{fetcher:async()=>new Response(null,{status:302,headers:{location:'https://evil.example'}})});
  assert.equal((await f.request('/bridge/api/v1/namespaces',{headers:authorized})).status,502);
});
test('login brute force is bounded and errors do not echo input',async t=>{
  const f=await fixture(t);
  for(let i=0;i<5;i++)assert.equal((await f.request('/workspace/login',{method:'POST',headers:{origin,'content-type':'application/json'},body:'{"token":"fixture-wrong"}'})).status,401);
  const r=await f.request('/workspace/login',{method:'POST',headers:{origin,'content-type':'application/json'},body:JSON.stringify({token})});
  assert.equal(r.status,429);assert.doesNotMatch(await r.text(),new RegExp(token));
});
test('oversized task body is rejected before upstream dispatch',async t=>{
  const f=await fixture(t);const r=await f.request('/dsh/rpc',{method:'POST',headers:authorized,body:'x'.repeat(1024*1024+1)});
  assert.equal(r.status,413);assert.equal(f.calls.length,0);
});
test('proxy streams assistant output without buffering until end',async t=>{
  let finish;
  const f=await fixture(t,{fetcher:async()=>new Response(new ReadableStream({start(controller){controller.enqueue(new TextEncoder().encode('data: first\n\n'));finish=()=>{controller.enqueue(new TextEncoder().encode('data: last\n\n'));controller.close();};}}),{headers:{'content-type':'text/event-stream'}})});
  await new Promise((resolve,reject)=>{
    let first=false, chunks='';
    const timeout=setTimeout(()=>{finish?.();reject(new Error('First streaming chunk was buffered'));},1000);
    const req=http.request(f.base+'/cpa/v1/responses',{method:'POST',headers:{host:new URL(origin).host,...authorized}},res=>{
      res.on('data',chunk=>{chunks+=chunk;if(!first){first=true;assert.match(chunks,/data: first/);finish();}});
      res.on('end',()=>{clearTimeout(timeout);assert.match(chunks,/data: last/);resolve();});res.on('error',reject);
    });req.on('error',reject);req.end('{}');
  });
});
