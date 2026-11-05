const $ = (id) => document.getElementById(id);
let state = { devices: [], buttons: [] };
let selected = '';
let sending = false;
let loading = false;
let formBusy = false;
let captureRun = null;

function element(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}

async function api(path, method = 'GET', body) {
  let response;
  try {
    response = await fetch(path, {
      method, headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'nature-remo' },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(12000),
    });
  } catch {
    throw new Error(path.endsWith('/send') ? '送信結果を確認できません。家電の状態を確認してから再操作してください。' : 'サーバーに接続できません。接続を確認して再度お試しください。');
  }
  const data = await response.json();
  if (!response.ok) {
    const error = new Error(data.error || '操作を完了できませんでした。');
    error.status = response.status;
    error.code = data.code;
    throw error;
  }
  return data;
}

function notice(message, isError = false) {
  $('notice').textContent = message;
  $('notice').className = isError ? 'error' : '';
  $('notice').hidden = false;
}

function openAdd(appliance = '', deviceId = '') {
  $('add-form').reset();
  $('add-form').querySelectorAll('details').forEach(el => { el.open = false; });
  $('capture-result').textContent = '';
  $('stop-capture').hidden = true;
  $('form-error').hidden = true;
  $('device').replaceChildren(...state.devices.map(d => new Option(d.name, d.id)));
  $('device').value = deviceId || selected || state.devices[0]?.id || '';
  $('appliance').value = appliance;
  $('appliance-list').replaceChildren(...[...new Set(state.buttons.map(b => b.appliance))].map(name => new Option(name)));
  $('add-dialog').showModal();
}

function render() {
  $('open-add').disabled = state.devices.length === 0;
  $('devices').replaceChildren();
  [{ id: '', name: 'すべての Remo' }, ...state.devices].forEach(d => {
    const tab = element('button', 'tab', d.name);
    tab.setAttribute('aria-pressed', String(selected === d.id));
    tab.onclick = () => { selected = d.id; render(); };
    $('devices').append(tab);
  });
  $('remotes').replaceChildren();
  const groups = new Map();
  state.buttons.filter(b => !selected || b.deviceId === selected).forEach(b => {
    const key = JSON.stringify([b.deviceId, b.appliance]);
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(b);
  });
  if (!groups.size) {
    const box = element('div', 'empty');
    box.append(element('span', 'empty-icon', '⌂'), element('h2', '', state.devices.length ? '最初のリモコンを登録しましょう' : 'Remo の接続設定を待っています'));
    box.append(element('p', '', state.devices.length ? '照明、テレビ、エアコン。必要なボタンの信号をあらかじめ保存しておくと、Nature のクラウド障害時にもこの画面から操作できます。' : 'サーバーの REMO_DEVICES に機器名と LAN 内の IP アドレスを設定してください。設定後、この画面を再読み込みできます。'));
    if (state.devices.length) { const add = element('button', 'primary', '＋ ボタンを登録'); add.onclick = () => openAdd(); box.append(add); }
    $('remotes').append(box);
  }
  groups.forEach(buttons => {
    const first = buttons[0];
    const device = state.devices.find(d => d.id === first.deviceId);
    const card = element('article', 'remote');
    const heading = element('div', 'remote-heading');
    const icon = /照明|ライト/.test(first.appliance) ? '☼' : /エアコン|空調/.test(first.appliance) ? '❄' : '⌁';
    const title = element('div');
    title.append(element('h2', '', first.appliance), element('p', 'device-name', device?.name || '接続先の Remo が設定されていません'));
    heading.append(element('span', 'remote-icon', icon), title);
    const grid = element('div', 'buttons');
    const status = element('p', 'card-status', '送信待ち');
    status.setAttribute('aria-live', 'polite');
    buttons.forEach(b => {
      const control = element('button', 'remote-button', b.name);
      control.dataset.available = String(!!device);
      control.disabled = !device || sending;
      control.onclick = async () => {
        if (sending) return;
        sending = true;
        document.querySelectorAll('.remote-button').forEach(el => { el.disabled = true; });
        status.textContent = `${b.name} を送信中…`;
        try {
          await api(`/api/buttons/${encodeURIComponent(b.id)}/send`, 'POST', {});
          status.textContent = `${b.name} · ${new Date().toLocaleTimeString('ja-JP')} に送信しました`;
        } catch (err) { status.textContent = '送信結果を確認できません'; notice(err.message, true); }
        finally {
          // 操作中のダブルクリックを抑える。サーバー側でも制限する。
          setTimeout(() => { sending = false; document.querySelectorAll('.remote-button').forEach(el => { el.disabled = el.dataset.available !== 'true'; }); }, 1100);
        }
      };
      grid.append(control);
    });
    const manage = element('details', 'card-manage');
    manage.append(element('summary', '', 'ボタンを管理'));
    buttons.forEach(b => {
      const row = element('div', 'manage-row');
      const remove = element('button', 'delete', '削除');
      remove.setAttribute('aria-label', `${b.appliance} の ${b.name} を削除`);
      remove.onclick = async () => {
        if (!confirm(`「${b.appliance} / ${b.name}」を削除しますか？`)) return;
        remove.disabled = true;
        try { await api(`/api/buttons/${encodeURIComponent(b.id)}`, 'DELETE', {}); notice('ボタンを削除しました。'); await refresh(); }
        catch (err) { notice(err.message, true); }
        finally { remove.disabled = false; }
      };
      row.append(element('span', '', b.name), remove); manage.append(row);
    });
    if (device) { const add = element('button', 'text-button', '＋ この家電にボタンを追加'); add.onclick = () => openAdd(first.appliance, first.deviceId); manage.append(add); }
    card.append(heading, grid, status, manage); $('remotes').append(card);
  });
}

async function refresh() {
  if (loading) return;
  loading = true; $('refresh').disabled = true;
  try {
    state = await api('/api/state');
    if (!state.devices.some(d => d.id === selected)) selected = '';
    render();
  } catch (err) { notice(err.message, true); }
  finally { loading = false; $('refresh').disabled = false; }
}

function busy(value) {
  formBusy = value;
  $('add-form').querySelectorAll('button,input,select,textarea').forEach(el => { el.disabled = value; });
}
$('add-dialog').addEventListener('cancel', e => { if (formBusy) e.preventDefault(); });
$('open-add').onclick = () => openAdd();
$('close-dialog').onclick = $('cancel').onclick = () => $('add-dialog').close();
$('refresh').onclick = refresh;
$('device').onchange = () => { $('signal').value = ''; $('capture-result').textContent = ''; };
$('signal').oninput = () => { $('capture-result').textContent = '貼り付けた信号を保存します。'; };
function acceptSignal(signal) {
  $('signal').value = JSON.stringify(signal);
  $('capture-result').textContent = `取得しました · ${signal.freq} kHz / ${signal.data.length} パルス。保存後、家電の反応を確認してください。`;
}

$('capture').onclick = async () => {
  const run = { cancelled: false };
  captureRun = run;
  busy(true); $('form-error').hidden = true; $('signal').value = '';
  $('capture-result').textContent = '待ち受けを準備しています。まだリモコンを押さないでください…';
  $('stop-capture').hidden = false; $('stop-capture').disabled = false;
  const readSignal = async () => {
    try { return await api(`/api/devices/${encodeURIComponent($('device').value)}/capture`, 'POST', {}); }
    catch (err) { if (err.status === 404 && err.code === 'no_signal') return null; throw err; }
  };
  try {
    // 開始時に残っていた信号を、新たに受信したものとして登録しない。
    let baseline = JSON.stringify(await readSignal());
    const deadline = Date.now() + 30000;
    while (!run.cancelled && Date.now() < deadline) {
      $('capture-result').textContent = `リモコンを押してください。受信待ち… 残り ${Math.ceil((deadline - Date.now()) / 1000)} 秒`;
      await new Promise(resolve => setTimeout(resolve, 1000));
      if (run.cancelled) break;
      const signal = await readSignal();
      if (run.cancelled) break;
      if (signal && JSON.stringify(signal) !== baseline) { acceptSignal(signal); return; }
      if (!signal) baseline = 'null';
    }
    if (run.cancelled) { $('capture-result').textContent = '待ち受けを中止しました。'; return; }
    throw new Error('新しい信号を確認できませんでした。向きと距離を確認して再度お試しください。同じ信号が残っている場合は、下の「直前の信号を取得する」から内容を確認できます。');
  } catch (err) { $('capture-result').textContent = ''; $('form-error').textContent = err.message; $('form-error').hidden = false; }
  finally { captureRun = null; busy(false); $('stop-capture').hidden = true; }
};
$('stop-capture').onclick = () => {
  if (captureRun) captureRun.cancelled = true;
  $('stop-capture').disabled = true;
  $('capture-result').textContent = '待ち受けを中止しています…';
};
$('capture-latest').onclick = async () => {
  busy(true); $('form-error').hidden = true; $('capture-result').textContent = '信号を取得しています…';
  $('signal').value = '';
  try {
    const signal = await api(`/api/devices/${encodeURIComponent($('device').value)}/capture`, 'POST', {});
    acceptSignal(signal);
  } catch (err) { $('capture-result').textContent = ''; $('form-error').textContent = err.message; $('form-error').hidden = false; }
  finally { busy(false); }
};
$('add-form').onsubmit = async e => {
  e.preventDefault();
  let signal;
  try { signal = JSON.parse($('signal').value); }
  catch { $('form-error').textContent = '信号を取得するか、有効な信号 JSON を貼り付けてください。'; $('form-error').hidden = false; return; }
  busy(true); $('form-error').hidden = true;
  try {
    await api('/api/buttons', 'POST', { deviceId: $('device').value, appliance: $('appliance').value, name: $('button-name').value, signal });
    $('add-dialog').close(); notice('ボタンを保存しました。家電の反応を確認しながら操作してください。'); await refresh();
  } catch (err) { $('form-error').textContent = err.message; $('form-error').hidden = false; }
  finally { busy(false); }
};
refresh();
