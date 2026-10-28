(function(){
var KEY='rc-theme',T=[['default','Default'],['terminal','Terminal'],['nocturne','Tokyo Night']],cur='default',root=document.documentElement;
try{cur=localStorage.getItem(KEY)||cur}catch(e){}
if(!T.some(function(t){return t[0]===cur}))cur='default';
function add(tag,attrs){var e=document.createElement(tag);for(var k in attrs)e[k]=attrs[k];document.head.append(e);return e}
add('link',{rel:'stylesheet',href:'assets/recon-theme-nocturne.css'});
add('link',{rel:'stylesheet',href:'assets/recon-theme-terminal.css'});
add('link',{rel:'stylesheet',href:'https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&display=swap'});
add('style',{textContent:'html[data-theme="default"] i.ph{display:none!important}button{text-transform:capitalize!important}button small,.fb .menu button small{text-transform:none!important}'});
function apply(){
root.setAttribute('data-theme',cur);
var off=cur==='default';
Array.prototype.forEach.call(document.querySelectorAll('link[rel=stylesheet]'),function(l){
var h=l.getAttribute('href')||'';
if(/(^|\/)(nocturne|recon-theme|recon-theme-nocturne|recon-theme-terminal)\.css$/.test(h)){
var isT=/terminal\.css$/.test(h);l.media=off||(isT&&cur!=='terminal')?'not all':'all'}});/* media, not disabled: a sheet disabled before it loads is never fetched */
try{localStorage.setItem(KEY,cur)}catch(e){}}
apply();
function build(){
var s=document.createElement('select');s.title='Theme';s.setAttribute('aria-label','Theme');
s.style.cssText='padding:4px 8px;font-size:12px;width:auto;margin:0';
T.forEach(function(t){var o=document.createElement('option');o.value=t[0];o.textContent=t[1];s.append(o)});
s.value=cur;s.onchange=function(){cur=s.value;apply()};
var nav=document.querySelector('nav'),r=document.getElementById('resetCols');
if(nav){nav.style.setProperty('display','flex','important');nav.style.setProperty('align-items','center');var w=document.createElement('span');w.style.cssText='display:inline-flex;align-items:center;gap:8px;';w.style.setProperty('margin-left','auto','important');if(r){r.style.marginLeft='0';r.style.float='none';nav.insertBefore(w,r);w.append(r,s)}else{w.append(s);nav.append(w)}}else{s.style.cssText+=';position:fixed;right:16px;top:16px;z-index:101';document.body.append(s)}}
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',build);else build();
})();
