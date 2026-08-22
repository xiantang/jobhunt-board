// 最小 DOM 桩：只够把页面脚本从头跑到尾，用来抓「一进页面就抛异常」这类错误。
// 不模拟渲染语义，只关心「有没有炸」和「往容器里写了东西没有」。
//
// 用法：node dom_shim.js <页面脚本> <喂给它的 JSON> <容器元素 id>
const fs = require('fs');
const [scriptPath, dataPath, rootID] = process.argv.slice(2);
const data = fs.readFileSync(dataPath, 'utf8');

const store = {};
function el(id) {
  if (store[id]) return store[id];
  store[id] = {
    id,
    // 页面用 <script type="application/json"> 传首屏数据，这里统一喂同一份。
    textContent: data,
    dataset: {},
    hidden: false,
    value: '',
    _html: '',
    set innerHTML(v) { this._html = v; },
    get innerHTML() { return this._html; },
    addEventListener() {},
    classList: { toggle() {}, add() {}, remove() {} },
    querySelector: () => null,
    querySelectorAll: () => [],
    focus() {},
  };
  return store[id];
}

global.document = {
  documentElement: { dataset: { board: 'JOBHUNT' } },
  getElementById: el,
  querySelector: (sel) => (sel === '[data-notice]' ? null : el('sel:' + sel)),
  querySelectorAll: () => [],
  body: { addEventListener() {} },
};
global.location = { search: '', pathname: '/', href: '' };
global.history = { replaceState() {} };
global.confirm = () => true;
global.API = { toast() {}, get: async () => ({}), post: async () => ({}), patch: async () => ({}), del: async () => ({}) };

new Function(fs.readFileSync(scriptPath, 'utf8'))();

const html = store[rootID] ? store[rootID].innerHTML : '';
if (!html) {
  console.error(`脚本跑完了，但没有往 #${rootID} 里写任何内容`);
  process.exit(1);
}
process.stdout.write(html);
