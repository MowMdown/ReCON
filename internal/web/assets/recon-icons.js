/* ReCON icons — adds Phosphor icons to buttons and nav links by their label, and tags primary/danger buttons.
   Survives the pages' dynamic re-rendering (MutationObserver). Drop-in: <script src="recon-icons.js" defer></script> */
(function () {
  var MAP = [
    ['message', 'chat-circle-text'], ['kick', 'sign-out'], ['unban', 'lock-key-open'], ['ban player', 'prohibit'], ['ban', 'prohibit'],
    ['details', 'identification-card'], ['import', 'download-simple'], ['export', 'upload-simple'],
    ['new flow', 'plus'], ['+ add step', 'plus-circle'], ['add step', 'plus-circle'], ['add', 'plus'], ['help', 'question'], ['values', 'brackets-curly'],
    ['cancel', 'x'], ['close', 'x'], ['save', 'floppy-disk'], ['send', 'paper-plane-tilt'], ['connect', 'plug'],
    ['reset', 'arrow-counter-clockwise'], ['confirm', 'check'], ['ok', 'check'], ['run', 'play'], ['edit', 'pencil-simple'],
    ['duplicate', 'copy'], ['copy', 'copy'], ['delete', 'trash'], ['remove', 'trash'], ['stop', 'stop'], ['console', 'terminal-window'],
    ['overview', 'squares-four'], ['sessions', 'clock-counter-clockwise'], ['ips', 'globe'], ['steam profile', 'steam-logo'],
    ['chat commands', 'terminal'], ['chat', 'chat-circle'], ['notes', 'note-pencil'], ['permissions', 'shield-check'], ['role', 'shield-check'], ['danger', 'warning'],
    ['steps', 'list-numbers'], ['if / else', 'git-fork'], ['triggers', 'lightning'], ['schedule', 'calendar-dots'],
    ['players', 'users'], ['server bans', 'prohibit'], ['flows', 'flow-arrow']
  ];
  var PRIMARY = /^(new flow|help|chat commands|commands|settings|console|save|send|connect|confirm|import|export|ban player|ok|run)\b/;
  var DANGER = /^(kick|ban|unban|delete|remove|stop)\b/;

  function pick(label) {
    label = label.trim().toLowerCase();
    for (var i = 0; i < MAP.length; i++) {
      var k = MAP[i][0];
      if (label === k || label.indexOf(k + ' ') === 0 || label.indexOf(k + '(') === 0) return MAP[i][1];
    }
    return null;
  }
  function decorate(el, kind) {
    if (el.closest('.menu')) { el.classList.remove('rc-primary', 'rc-danger'); var o = el.querySelector(':scope > i.ph'); if (o) o.remove(); return; }
    var label = el.textContent;
    if (!label.trim()) return;
    // Primary/danger by label, also for buttons with their own icon (Commands,
    // Settings, Console), but not the Commands dialog's big buttons or tabs
    // (Help's Chat commands tab, the Details tabs), which have their own look,
    // or buttons that set their own (data-rc, e.g. Delete Player's Yes).
    if (kind === 'button' && !el.closest('.srvlist, .tabs, .dtabs, [data-rc]')) {
      var l = label.trim().toLowerCase();
      el.classList.toggle('rc-danger', DANGER.test(l));
      el.classList.toggle('rc-primary', !DANGER.test(l) && PRIMARY.test(l));
    }
    var first = el.firstElementChild;
    if (first && first.classList.contains('ph')) return;
    if (el.querySelector('svg')) return; // already has its own icon
    var name = pick(label);
    if (!name) return;
    var i = document.createElement('i');
    i.className = 'ph ph-' + name;
    i.setAttribute('aria-hidden', 'true');
    el.insertBefore(i, el.firstChild);
    if (/^\+\s/.test(el.lastChild.nodeValue || '')) el.lastChild.nodeValue = el.lastChild.nodeValue.replace(/^\+\s/, '');
  }
  var queued = false;
  function sweep() {
    queued = false;
    document.querySelectorAll('button').forEach(function (b) { decorate(b, 'button'); });
    document.querySelectorAll('nav a').forEach(function (a) { decorate(a, 'link'); });
  }
  function schedule() { if (!queued) { queued = true; requestAnimationFrame(sweep); } }
  function start() {
    sweep();
    new MutationObserver(schedule).observe(document.body, { childList: true, subtree: true, characterData: true });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start); else start();
})();
