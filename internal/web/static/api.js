// fetch 封装 + toast 提示：所有接口错误都走同一条展示路径。
const API = (() => {
  const toastEl = document.getElementById('toast');
  let toastTimer = null;

  function toast(message, ok = false) {
    toastEl.textContent = message;
    toastEl.classList.toggle('toast--ok', ok);
    toastEl.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toastEl.hidden = true; }, 3200);
  }

  // request 在失败时抛出带 code/message 的错误，调用方决定是否回滚 UI。
  async function request(method, url, body) {
    let res;
    try {
      res = await fetch(url, {
        method,
        headers: body ? { 'Content-Type': 'application/json' } : undefined,
        body: body ? JSON.stringify(body) : undefined,
      });
    } catch (e) {
      const err = new Error('网络异常，请检查服务是否在运行');
      err.code = 'NETWORK';
      throw err;
    }

    const text = await res.text();
    const data = text ? JSON.parse(text) : {};
    if (!res.ok) {
      const err = new Error((data.error && data.error.message) || `请求失败（${res.status}）`);
      err.code = (data.error && data.error.code) || 'UNKNOWN';
      err.field = data.error && data.error.field;
      throw err;
    }
    return data;
  }

  return {
    toast,
    get: (url) => request('GET', url),
    post: (url, body) => request('POST', url, body),
    patch: (url, body) => request('PATCH', url, body),
    del: (url) => request('DELETE', url),
  };
})();
