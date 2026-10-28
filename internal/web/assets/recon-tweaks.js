(function(){
var KEY='rc-flows-tweaks',D={h:30,px:10},S=Object.assign({},D);
try{Object.assign(S,JSON.parse(localStorage.getItem(KEY)||'{}'))}catch(e){}
function apply(){var r=document.documentElement.style;r.setProperty('--rc-act-h',S.h+'px');r.setProperty('--rc-act-px',S.px+'px');try{localStorage.setItem(KEY,JSON.stringify(S))}catch(e){}}
apply();
function build(){
var b=document.createElement('button');b.type='button';b.title='Tweaks';b.innerHTML='<i class="ph ph-sliders-horizontal"></i>Tweaks';
b.style.cssText='padding:4px 10px;font-size:12px;margin:0';
var p=document.createElement('div');p.hidden=true;
p.style.cssText='position:fixed;right:24px;top:64px;z-index:50;width:240px;padding:14px;background:var(--color-surface,#222);border-radius:8px;box-shadow:var(--shadow-lg,0 6px 24px #000a);display:flex;flex-direction:column;gap:12px;font-size:13px';
p.innerHTML='<b style="font-weight:500">Tweaks</b>';
function row(label,key,min,max){var l=document.createElement('label');l.style.cssText='display:flex;flex-direction:column;gap:4px;color:var(--color-neutral-300,#ccc)';
var v=document.createElement('span');v.style.cssText='display:flex;justify-content:space-between';v.innerHTML='<span>'+label+'</span><span></span>';
var i=document.createElement('input');i.type='range';i.min=min;i.max=max;i.value=S[key];i.style.cssText='padding:0;border:0!important;accent-color:var(--color-accent,#8ab4f8)';
function show(){v.lastChild.textContent=S[key]+'px'}show();
i.oninput=function(){S[key]=+i.value;show();apply()};l.append(v,i);return l}
var rs=document.createElement('button');rs.type='button';rs.textContent='Reset';rs.onclick=function(){S=Object.assign({},D);apply();p.remove();b.remove();build()};
p.append(row('Action button height','h',24,44),row('Action button padding','px',6,20),rs);
b.onclick=function(){p.hidden=!p.hidden};var sp=document.querySelector('nav > span');if(sp)sp.prepend(b);else{b.style.cssText+=';position:fixed;right:16px;bottom:16px;z-index:50';document.body.append(b)}document.body.append(p)}
if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',build);else build();
})();
