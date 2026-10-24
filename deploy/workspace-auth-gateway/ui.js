let csrf = '', provider = '', state = '';
const $ = id => document.getElementById(id);
const show = value => { $('output').textContent = JSON.stringify(value, null, 2); };
async function api(path, payload) {
  const response = await fetch(path, { method: payload === undefined ? 'GET' : 'POST', credentials: 'same-origin',
    headers: payload === undefined ? {} : { 'content-type': 'application/json', 'x-workspace-csrf': csrf },
    ...(payload === undefined ? {} : { body: JSON.stringify(payload) }) });
  const data = await response.json();
  if (response.status === 401) { $('panel').hidden = true; $('login').hidden = false; csrf = ''; }
  if (!response.ok) throw new Error(data.error || '请求失败');
  return data;
}
function ready(data) { csrf = data.csrf; $('panel').hidden = false; $('login').hidden = true; show({ connected: true }); }
$('login').onsubmit = async event => {
  event.preventDefault(); const token = $('token').value; $('token').value = '';
  try { ready(await api('/workspace/login', { token })); } catch (error) { show({ error: error.message }); }
};
$('logout').onclick = async () => {
  try { await api('/workspace/logout', {}); csrf = ''; state = ''; provider = ''; $('oauth-link').hidden = true; $('panel').hidden = true; $('login').hidden = false; show({ loggedOut: true }); }
  catch (error) { show({ error: error.message }); }
};
document.querySelectorAll('[data-path]').forEach(button => button.onclick = async () => {
  try { show(await api(button.dataset.path)); } catch (error) { show({ error: error.message }); }
});
document.querySelectorAll('[data-provider]').forEach(button => button.onclick = async () => {
  try {
    provider = button.dataset.provider; state = ''; $('oauth-link').hidden = true;
    const data = await api('/cpa-management/oauth/auth-url?provider=' + encodeURIComponent(provider) + '&is_webui=true');
    const url = new URL(data.url);
    if (url.protocol !== 'https:' || !data.state) throw new Error('授权地址无效');
    state = data.state; $('oauth-link').href = url.href; $('oauth-link').hidden = false; show({ authorizationStarted: true, provider });
  } catch (error) { show({ error: error.message }); }
});
$('oauth-status').onclick = async () => {
  try { if (!state) throw new Error('请先启动账号授权'); show(await api('/cpa-management/oauth/status?state=' + encodeURIComponent(state))); }
  catch (error) { show({ error: error.message }); }
};
$('callback').onsubmit = async event => {
  event.preventDefault(); const redirect_url = $('redirect').value; $('redirect').value = '';
  try { if (!state || !provider) throw new Error('请先启动账号授权'); const parsed = new URL(redirect_url); if (parsed.searchParams.get('state') !== state) throw new Error('回调与本次授权不匹配'); show(await api('/cpa-management/oauth/callback', { provider, redirect_url })); }
  catch (error) { show({ error: error.message }); }
};
$('channel').onsubmit = async event => {
  event.preventDefault(); const id = $('channel-id').value;
  try { if (!/^[1-9][0-9]*$/.test(id)) throw new Error('通道 ID 无效'); show(await api('/newapi/api/channel/' + id + '/status', { status: Number($('channel-status').value) })); }
  catch (error) { show({ error: error.message }); }
};
api('/workspace/session').then(ready).catch(() => {});
