// ยืนยันก่อนส่งฟอร์มที่มี data-confirm (แทน onclick inline เพื่อให้ CSP เข้มงวดได้)
document.addEventListener('submit', function (e) {
  var f = e.target;
  var msg = f.getAttribute && f.getAttribute('data-confirm');
  if (msg && !window.confirm(msg)) e.preventDefault();
});