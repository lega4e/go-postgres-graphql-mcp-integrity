var Z=Object.defineProperty;var G=(o,e,t)=>e in o?Z(o,e,{enumerable:!0,configurable:!0,writable:!0,value:t}):o[e]=t;var i=(o,e,t)=>G(o,typeof e!="symbol"?e+"":e,t);(function(){const e=document.createElement("link").relList;if(e&&e.supports&&e.supports("modulepreload"))return;for(const r of document.querySelectorAll('link[rel="modulepreload"]'))a(r);new MutationObserver(r=>{for(const s of r)if(s.type==="childList")for(const l of s.addedNodes)l.tagName==="LINK"&&l.rel==="modulepreload"&&a(l)}).observe(document,{childList:!0,subtree:!0});function t(r){const s={};return r.integrity&&(s.integrity=r.integrity),r.referrerPolicy&&(s.referrerPolicy=r.referrerPolicy),r.crossOrigin==="use-credentials"?s.credentials="include":r.crossOrigin==="anonymous"?s.credentials="omit":s.credentials="same-origin",s}function a(r){if(r.ep)return;r.ep=!0;const s=t(r);fetch(r.href,s)}})();const ee=`
  :host {
    box-sizing: border-box;
    font-family: var(--ga-font-sans, ui-sans-serif, system-ui, sans-serif);
  }
  :host([hidden]) { display: none !important; }
  *, *::before, *::after { box-sizing: inherit; }
  @media (prefers-reduced-motion: reduce) {
    * { transition-duration: 0.001ms !important; animation-duration: 0.001ms !important; }
  }
`;class d extends HTMLElement{static get observedAttributes(){return this.observed}static get _css(){return Object.prototype.hasOwnProperty.call(this,"_cssCache")||(this._cssCache=ee+(this.styles||"")),this._cssCache}constructor(){super(),this.attachShadow({mode:"open",delegatesFocus:!0}),this._mounted=!1}connectedCallback(){this._mounted=!0,this.render()}attributeChangedCallback(){this._mounted&&this.render()}template(){return""}render(){this.shadowRoot.innerHTML="<style>"+this.constructor._css+"</style>"+this.template()}$(e){return this.shadowRoot.querySelector(e)}emit(e,t){this.dispatchEvent(new CustomEvent(e,{detail:t,bubbles:!0,composed:!0}))}hasFlag(e){return this.hasAttribute(e)}attr(e,t=""){return this.getAttribute(e)??t}}i(d,"styles",""),i(d,"observed",[]);function c(o,e){customElements.get(o)||customElements.define(o,e)}function n(o){return String(o??"").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;")}class q extends d{constructor(){super(...arguments);i(this,"_guard",t=>{(this.hasFlag("disabled")||this.hasFlag("loading"))&&(t.stopImmediatePropagation(),t.preventDefault())})}connectedCallback(){super.connectedCallback(),this.addEventListener("click",this._guard,!0)}disconnectedCallback(){this.removeEventListener("click",this._guard,!0)}_pass(t,a=t){return this.hasAttribute(t)?` ${a}="${n(this.getAttribute(t))}"`:""}template(){const t=this.attr("href"),a=t?"a":"button",r=this._pass("aria-label"),s=t?`href="${n(t)}"`+this._pass("download")+this._pass("target")+this._pass("rel")+r:`type="${n(this.attr("type","button"))}"`+this._pass("name")+r+(this.hasFlag("disabled")?" disabled":""),l=this.hasFlag("loading")?'<span class="spinner" aria-hidden="true"></span>':"";return`
      <${a} class="btn" part="button" ${s}>
        <slot name="start"></slot>
        ${l}
        <slot></slot>
        <slot name="end"></slot>
      </${a}>
    `}}i(q,"observed",["variant","size","href","download","target","rel","type","name","aria-label","disabled","loading","block"]),i(q,"styles",`
    :host { display: inline-block; }
    :host([block]) { display: block; }

    .btn {
      --_bg: var(--ga-bg-elev, #1a1a1a);
      --_fg: var(--ga-fg, #ededed);
      --_bd: var(--ga-border-strong, #2a2a2a);
      --_bg-hover: var(--ga-bg-elev-hover, #1f1f1f);

      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: var(--ga-space-2, 8px);
      width: 100%;
      font-family: inherit;
      font-weight: 500;
      line-height: 1;
      white-space: nowrap;
      text-decoration: none;
      cursor: pointer;
      border: 1px solid var(--_bd);
      border-radius: var(--ga-radius, 6px);
      background: var(--_bg);
      color: var(--_fg);
      transition: background var(--ga-transition, 0.18s ease),
        border-color var(--ga-transition, 0.18s ease),
        filter var(--ga-transition, 0.18s ease),
        transform var(--ga-transition, 0.18s ease);
    }
    .btn:hover { background: var(--_bg-hover); }
    .btn:active { transform: translateY(1px); }
    .btn:focus-visible {
      outline: none;
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
    }

    /* sizes */
    :host([size="sm"]) .btn { font-size: var(--ga-fs-sm, 14px); padding: 6px 12px; height: 32px; }
    .btn { font-size: var(--ga-fs-sm, 14px); padding: 8px 16px; height: 40px; }
    :host([size="lg"]) .btn { font-size: var(--ga-fs-base, 17px); padding: 12px 22px; height: 48px; }

    /* variants */
    :host([variant="primary"]) .btn {
      --_bg: var(--ga-accent, #54a2ff);
      --_fg: var(--ga-accent-contrast, #000);
      --_bd: var(--ga-accent, #54a2ff);
    }
    :host([variant="primary"]) .btn:hover { background: var(--ga-accent, #54a2ff); filter: brightness(1.1); }

    :host([variant="ghost"]) .btn {
      --_bg: transparent;
      --_bd: transparent;
    }
    :host([variant="ghost"]) .btn:hover { background: var(--ga-bg-elev, #1a1a1a); }

    :host([variant="danger"]) .btn {
      --_bg: transparent;
      --_fg: var(--ga-red, #ff6568);
      --_bd: color-mix(in srgb, var(--ga-red, #ff6568) 40%, transparent);
    }
    :host([variant="danger"]) .btn:hover {
      background: color-mix(in srgb, var(--ga-red, #ff6568) 12%, transparent);
    }

    :host([disabled]) .btn,
    :host([loading]) .btn {
      opacity: 0.5;
      pointer-events: none;
      cursor: not-allowed;
    }

    .spinner {
      width: 1em; height: 1em;
      border: 2px solid currentColor;
      border-right-color: transparent;
      border-radius: 50%;
      animation: spin 0.6s linear infinite;
    }
    @keyframes spin { to { transform: rotate(360deg); } }

    ::slotted([slot="start"]), ::slotted([slot="end"]) { display: inline-flex; }
  `);c("ga-button",q);class f extends d{constructor(){var e;super(),this._internals=(e=this.attachInternals)==null?void 0:e.call(this)}_parse(){try{return JSON.parse(this.attr("items","[]"))}catch{return[]}}template(){var r;const e=this._parse(),t=this.attr("value")||((r=e[0])==null?void 0:r.id);return`<div class="group" part="group" role="radiogroup">${e.map(s=>{const l=s.id===t,p=l?"0":"-1";return s.href?`<a class="item" part="item" data-id="${n(s.id)}"
          href="${n(s.href)}" role="radio"
          aria-checked="${l}"
          ${l?'aria-current="page"':""}
          tabindex="${p}">${n(s.label)}</a>`:`<button class="item" part="item" type="button" data-id="${n(s.id)}"
        role="radio" aria-checked="${l}" tabindex="${p}">${n(s.label)}</button>`}).join("")}</div>`}render(){var t;super.render(),(t=this._internals)==null||t.setFormValue(this.value);const e=[...this.shadowRoot.querySelectorAll(".item")];e.forEach(a=>{a.tagName==="BUTTON"&&a.addEventListener("click",()=>this._select(a.dataset.id)),a.addEventListener("keydown",r=>this._onKey(r,e))})}_onKey(e,t){const a={ArrowRight:1,ArrowDown:1,ArrowLeft:-1,ArrowUp:-1}[e.key];if(!a)return;e.preventDefault();const r=t.indexOf(e.currentTarget),s=t[(r+a+t.length)%t.length];s&&(s.focus(),s.tagName==="BUTTON"&&this._select(s.dataset.id))}_select(e){e==null||e===this.attr("value")||(this.setAttribute("value",e),this.emit("change",{value:e}))}get value(){var e;return this.attr("value")||((e=this._parse()[0])==null?void 0:e.id)||""}set value(e){this.setAttribute("value",e)}}i(f,"formAssociated",!0),i(f,"observed",["items","value"]),i(f,"styles",`
    :host { display: inline-block; }
    .group {
      display: inline-flex;
      gap: var(--ga-space-1, 4px);
      padding: 3px;
      background: var(--ga-bg-elev, #1a1a1a);
      border: 1px solid var(--ga-border-strong, #2a2a2a);
      border-radius: var(--ga-radius-full, 9999px);
    }
    .item {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: var(--ga-space-2, 8px);
      font-family: inherit;
      font-size: var(--ga-fs-sm, 14px);
      font-weight: 500;
      line-height: 1;
      white-space: nowrap;
      text-decoration: none;
      padding: 7px 16px;
      border: 1px solid transparent;
      border-radius: var(--ga-radius-full, 9999px);
      background: transparent;
      color: var(--ga-muted, #878787);
      cursor: pointer;
      transition: background var(--ga-transition, 0.18s ease),
        color var(--ga-transition, 0.18s ease),
        border-color var(--ga-transition, 0.18s ease);
    }
    .item:hover { color: var(--ga-fg, #ededed); }
    .item[aria-checked="true"],
    .item[aria-current="page"] {
      background: var(--ga-fg, #ededed);
      color: var(--ga-bg, #000);
      border-color: var(--ga-fg, #ededed);
    }
    .item:focus-visible {
      outline: none;
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
    }
  `);c("ga-radio-group",f);class M extends d{_pass(e,t=e){return this.hasAttribute(e)?` ${t}="${n(this.getAttribute(e))}"`:""}template(){const e=this.attr("href"),t=this.attr("prompt"),a=t?`<span class="prompt" part="prompt" aria-hidden="true">${n(t)}</span>`:"",r='<code class="text" part="text"><slot></slot></code>';return e?`
        <a class="block" part="block"
          href="${n(e)}"${this._pass("target")}${this._pass("rel")}>
          ${a}
          ${r}
          <span class="arrow" part="arrow" aria-hidden="true">${ae}</span>
        </a>
      `:`
      <div class="block" part="block">
        ${a}
        ${r}
        <button class="action" part="copy" type="button" aria-label="Copy to clipboard">
          <span class="ico">${Q}</span>
        </button>
      </div>
    `}render(){super.render();const e=this.$("button.action");e==null||e.addEventListener("click",()=>this._copy(e))}_copy(e){var r;const t=(this.textContent||"").trim(),a=()=>{e.classList.add("copied"),e.querySelector(".ico").innerHTML=te,this.emit("copy",{text:t}),clearTimeout(this._t),this._t=setTimeout(()=>{e.classList.remove("copied");const s=e.querySelector(".ico");s&&(s.innerHTML=Q)},1500)};(r=navigator.clipboard)!=null&&r.writeText?navigator.clipboard.writeText(t).then(a).catch(()=>{}):a()}disconnectedCallback(){clearTimeout(this._t)}}i(M,"observed",["prompt","href","target","rel"]),i(M,"styles",`
    :host { display: block; }
    .block {
      display: flex;
      align-items: center;
      gap: var(--ga-space-3, 12px);
      font-family: var(--ga-font-mono, ui-monospace, monospace);
      font-size: var(--ga-fs-sm, 14px);
      line-height: 1.5;
      color: var(--ga-fg, #ededed);
      text-decoration: none;
      background: var(--ga-bg-elev, #1a1a1a);
      border: 1px solid var(--ga-border-strong, #2a2a2a);
      border-radius: var(--ga-radius, 6px);
      padding: 10px 12px;
    }
    a.block { transition: border-color var(--ga-transition, 0.18s ease),
      background var(--ga-transition, 0.18s ease); }
    a.block:hover { border-color: var(--ga-dim, #454545); background: var(--ga-bg-elev-hover, #1f1f1f); }
    a.block:focus-visible {
      outline: none;
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
    }
    .prompt { color: var(--ga-muted, #878787); user-select: none; flex: none; }
    .text {
      flex: 1 1 auto;
      min-width: 0;
      overflow-x: auto;
      white-space: pre;
      -ms-overflow-style: none;
      scrollbar-width: none;
    }
    .text::-webkit-scrollbar { display: none; }
    .action {
      flex: none;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 28px; height: 28px;
      margin: -4px -2px -4px 0;
      color: var(--ga-muted, #878787);
      background: transparent;
      border: none;
      border-radius: var(--ga-radius, 6px);
      cursor: pointer;
      font: inherit;
      transition: color var(--ga-transition, 0.18s ease),
        background var(--ga-transition, 0.18s ease);
    }
    button.action:hover { color: var(--ga-fg, #ededed); background: var(--ga-bg-elev-hover, #1f1f1f); }
    button.action:focus-visible {
      outline: none;
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
    }
    .action.copied { color: var(--ga-green, #00c758); }
    .arrow { flex: none; color: var(--ga-muted, #878787); }
    svg { display: block; }
  `);const Q=`<svg width="16" height="16" viewBox="0 0 24 24" fill="none"
  stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
  <rect x="9" y="9" width="13" height="13" rx="2"></rect>
  <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>`,te=`<svg width="16" height="16" viewBox="0 0 24 24" fill="none"
  stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
  <path d="M20 6 9 17l-5-5"></path></svg>`,ae=`<svg width="14" height="14" viewBox="0 0 24 24" fill="none"
  stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
  <path d="M7 17 17 7"></path><path d="M7 7h10v10"></path></svg>`;c("ga-code",M);class T extends d{_parse(){try{return JSON.parse(this.attr("items","[]"))}catch{return[]}}template(){const e=this._parse(),t=e.length-1;return`<nav aria-label="Breadcrumb" part="nav"><ol part="list">${e.map((r,s)=>{const l=s>0?'<span class="sep" part="separator" aria-hidden="true">/</span>':"",p=s===t,b=n(r.label),v=!p&&r.href?`<a part="link" href="${n(r.href)}">${b}</a>`:`<span class="current" part="current" ${p?'aria-current="page"':""}>${b}</span>`;return`<li>${l}${v}</li>`}).join("")}</ol></nav>`}}i(T,"observed",["items"]),i(T,"styles",`
    :host { display: block; }
    ol {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: var(--ga-space-2, 8px);
      margin: 0;
      padding: 0;
      list-style: none;
      font-family: var(--ga-font-mono, ui-monospace, monospace);
      font-size: var(--ga-fs-sm, 14px);
    }
    li { display: inline-flex; align-items: center; gap: var(--ga-space-2, 8px); }
    a {
      color: var(--ga-muted, #878787);
      text-decoration: none;
      transition: color var(--ga-transition, 0.18s ease);
    }
    a:hover { color: var(--ga-fg, #ededed); }
    a:focus-visible {
      outline: none;
      border-radius: var(--ga-radius, 6px);
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
    }
    .current { color: var(--ga-fg, #ededed); }
    .sep { color: var(--ga-dim, #454545); user-select: none; }
  `);c("ga-breadcrumbs",T);let re=0;class j extends d{connectedCallback(){this._scope="ga-table-"+re++,this.setAttribute("data-ga-scope",this._scope),super.connectedCallback()}disconnectedCallback(){var e;(e=this._sheet)==null||e.remove(),this._sheet=null}_parse(){try{return JSON.parse(this.attr("columns","[]"))}catch{return[]}}template(){const e=this._parse(),t=e.map(r=>r.width||"minmax(0, 1fr)").join(" ")||"1fr";this.style.setProperty("--ga-table-cols",t);const a=e.map(r=>{const s="cell"+(r.mono?" mono":""),l=r.align?`text-align:${n(r.align)}`:"";return`<span class="${s}" part="head-cell" style="${l}">${n(r.label??"")}</span>`}).join("");return this._applyCellRules(e),`
      <div class="table" part="table">
        <div class="head" part="header" role="row">${a}</div>
        <slot part="body"></slot>
      </div>
    `}_applyCellRules(e){const t=e.map((a,r)=>{const s=[];return a.align&&s.push(`text-align:${a.align}`),a.mono&&s.push("font-family:var(--ga-font-mono, ui-monospace, monospace);font-variant-numeric:tabular-nums"),s.length?`ga-table[data-ga-scope="${this._scope}"] > *:not([slot]) > :nth-child(${r+1}){${s.join(";")}}`:""}).filter(Boolean).join(`
`);this._sheet||(this._sheet=document.createElement("style"),document.head.appendChild(this._sheet)),this._sheet.textContent=t}}i(j,"observed",["columns"]),i(j,"styles",`
    :host { display: block; }
    .table {
      border: 1px solid var(--ga-border, #1a1a1a);
      border-radius: var(--ga-radius-lg, 8px);
      overflow: hidden;
    }
    .head {
      display: grid;
      grid-template-columns: var(--ga-table-cols, 1fr);
      align-items: center;
      gap: var(--ga-space-4, 16px);
      padding: 10px var(--ga-space-4, 16px);
      background: color-mix(in srgb, var(--ga-bg-elev, #1a1a1a) 40%, transparent);
      border-bottom: 1px solid var(--ga-border, #1a1a1a);
      font-size: var(--ga-fs-xs, 12px);
      font-weight: 600;
      letter-spacing: 0.02em;
      text-transform: uppercase;
      color: var(--ga-muted, #878787);
    }
    .head .mono { font-family: var(--ga-font-mono, ui-monospace, monospace); text-transform: none; }

    /* Slotted rows share the column grid and get borders + hover. */
    ::slotted(*) {
      display: grid !important;
      grid-template-columns: var(--ga-table-cols, 1fr);
      align-items: center;
      gap: var(--ga-space-4, 16px);
      padding: var(--ga-space-3, 12px) var(--ga-space-4, 16px);
      border-top: 1px solid var(--ga-border, #1a1a1a);
      color: var(--ga-fg, #ededed);
      text-decoration: none;
      transition: background var(--ga-transition, 0.18s ease);
    }
    ::slotted(:first-child) { border-top: none; }
    ::slotted(*:hover) { background: var(--ga-bg-elev-hover, #1f1f1f); }
    ::slotted(a) { cursor: pointer; }
    ::slotted(a:focus-visible) {
      outline: none;
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
    }
  `);c("ga-table",j);class F extends d{template(){return'<span class="badge" part="badge"><slot></slot></span>'}}i(F,"observed",["color","solid","size"]),i(F,"styles",`
    :host { display: inline-block; }
    .badge {
      --_c: var(--ga-muted, #878787);
      display: inline-flex;
      align-items: center;
      gap: 6px;
      font-family: var(--ga-font-sans, ui-sans-serif, system-ui, sans-serif);
      font-size: var(--ga-fs-xs, 12px);
      font-weight: 500;
      line-height: 1;
      padding: 2px 10px;
      border-radius: var(--ga-radius-full, 9999px);
      border: 1px solid var(--ga-border, #1a1a1a);
      color: var(--_c);
      background: transparent;
      white-space: nowrap;
    }
    :host([size="sm"]) .badge { font-size: 11px; padding: 1px 8px; }

    /* Colored variants: tint the text + border, keep the fill subtle. */
    :host([color="blue"])   .badge { --_c: var(--ga-blue, #54a2ff); }
    :host([color="green"])  .badge { --_c: var(--ga-green, #00c758); }
    :host([color="amber"])  .badge { --_c: var(--ga-amber, #fcbb00); }
    :host([color="purple"]) .badge { --_c: var(--ga-purple, #ac4bff); }
    :host([color="red"])    .badge { --_c: var(--ga-red, #ff6568); }
    :host([color="blue"])   .badge,
    :host([color="green"])  .badge,
    :host([color="amber"])  .badge,
    :host([color="purple"]) .badge,
    :host([color="red"])    .badge {
      border-color: color-mix(in srgb, var(--_c) 40%, transparent);
    }

    :host([solid]) .badge {
      background: var(--_c);
      color: var(--ga-accent-contrast, #000);
      border-color: var(--_c);
    }
  `);c("ga-badge",F);class O extends d{connectedCallback(){super.connectedCallback(),this._sync=()=>this._toggleSlots(),this.shadowRoot.addEventListener("slotchange",this._sync)}_toggleSlots(){for(const e of["header","footer"]){const t=this.$(`slot[name="${e}"]`),a=this.$(`.${e}`);t&&a&&a.classList.toggle("show",t.assignedNodes().length>0)}}template(){const e=this.attr("href"),t=e?"a":"div",a=e?`href="${e}"`:"";return`
      <${t} class="card" part="card" ${a}>
        <div class="header" part="header"><slot name="header"></slot></div>
        <div class="body" part="body"><slot></slot></div>
        <div class="footer" part="footer"><slot name="footer"></slot></div>
      </${t}>
    `}}i(O,"observed",["interactive","href","padding"]),i(O,"styles",`
    :host { display: block; }
    .card {
      display: flex;
      flex-direction: column;
      gap: var(--ga-space-3, 12px);
      color: var(--ga-fg, #ededed);
      text-decoration: none;
      background: color-mix(in srgb, var(--ga-bg-elev, #1a1a1a) 30%, transparent);
      border: 1px solid var(--ga-border, #1a1a1a);
      border-radius: var(--ga-radius-lg, 8px);
      overflow: hidden;
      transition: background var(--ga-transition, 0.18s ease),
        border-color var(--ga-transition, 0.18s ease);
    }
    :host([interactive]) .card,
    :host([href]) .card { cursor: pointer; }
    :host([interactive]) .card:hover,
    :host([href]) .card:hover {
      background: color-mix(in srgb, var(--ga-bg-elev, #1a1a1a) 60%, transparent);
      border-color: var(--ga-dim, #454545);
    }
    :host([href]) .card:focus-visible {
      outline: none;
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
    }

    /* Slotted title turns accent-blue on hover (like the project cards). */
    ::slotted(h3), ::slotted(strong) { transition: color var(--ga-transition, 0.18s ease); }
    :host([interactive]) .card:hover ::slotted(h3),
    :host([interactive]) .card:hover ::slotted(strong),
    :host([href]) .card:hover ::slotted(h3),
    :host([href]) .card:hover ::slotted(strong) { color: var(--ga-accent, #54a2ff); }

    .body { padding: var(--ga-space-5, 20px); }
    :host([padding="none"]) .body { padding: 0; }
    :host([padding="sm"]) .body { padding: var(--ga-space-3, 12px); }
    :host([padding="lg"]) .body { padding: var(--ga-space-8, 32px); }

    .header, .footer { display: none; }
    .header.show, .footer.show { display: block; }
    .header {
      padding: var(--ga-space-4, 16px) var(--ga-space-5, 20px);
      border-bottom: 1px solid var(--ga-border, #1a1a1a);
      font-weight: 600;
    }
    .footer {
      padding: var(--ga-space-4, 16px) var(--ga-space-5, 20px);
      border-top: 1px solid var(--ga-border, #1a1a1a);
      color: var(--ga-muted, #878787);
      font-size: var(--ga-fs-sm, 14px);
    }
    /* Collapse the gap when only the body is present. */
    .card:not(:has(.header.show)):not(:has(.footer.show)) { gap: 0; }
  `);c("ga-card",O);class S extends d{_initials(e){return e.trim().split(/\s+/).slice(0,2).map(a=>{var r;return((r=a[0])==null?void 0:r.toUpperCase())??""}).join("")||"?"}template(){const e=this.attr("src"),t=this.attr("name",""),a=e?`<img src="${n(e)}" alt="${n(t)}" loading="lazy" />`:`<span aria-hidden="true">${n(this._initials(t))}</span>`;return`<div class="avatar" part="avatar" role="img" aria-label="${n(t)}">${a}</div>`}}i(S,"observed",["src","name","size","square"]),i(S,"styles",`
    :host { display: inline-block; }
    .avatar {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 40px; height: 40px;
      overflow: hidden;
      font-family: var(--ga-font-mono, ui-monospace, monospace);
      font-size: var(--ga-fs-sm, 14px);
      font-weight: 600;
      color: var(--ga-fg, #ededed);
      background: var(--ga-bg-elev, #1a1a1a);
      border: 1px solid var(--ga-border-strong, #2a2a2a);
      border-radius: var(--ga-radius-full, 9999px);
      user-select: none;
    }
    :host([square]) .avatar { border-radius: var(--ga-radius, 6px); }
    :host([size="sm"]) .avatar { width: 28px; height: 28px; font-size: 11px; }
    :host([size="lg"]) .avatar { width: 64px; height: 64px; font-size: var(--ga-fs-lg, 20px); }
    img { width: 100%; height: 100%; object-fit: cover; display: block; }
  `);c("ga-avatar",S);class x extends d{constructor(){var e;super(),this._internals=(e=this.attachInternals)==null?void 0:e.call(this)}template(){const e=this.attr("label"),t=this.attr("error"),a=this.attr("hint"),r=this.hasFlag("required")?'<span class="req">*</span>':"";return`
      <div class="field">
        ${e?`<label part="label">${n(e)}${r}</label>`:""}
        <input
          part="input"
          type="${n(this.attr("type","text"))}"
          placeholder="${n(this.attr("placeholder"))}"
          value="${n(this.attr("value"))}"
          ${this.hasFlag("disabled")?"disabled":""}
          ${this.hasFlag("required")?"required":""}
          aria-invalid="${t?"true":"false"}"
        />
        ${t?`<span class="error" part="error">${n(t)}</span>`:a?`<span class="hint" part="hint">${n(a)}</span>`:""}
      </div>
    `}render(){super.render();const e=this.$("input");e&&(e.addEventListener("input",()=>{var t;this._value=e.value,(t=this._internals)==null||t.setFormValue(e.value),this.emit("input",{value:e.value})}),e.addEventListener("change",()=>this.emit("change",{value:e.value})))}get value(){var e;return((e=this.$("input"))==null?void 0:e.value)??this._value??this.attr("value")}set value(e){this._value=e,this.setAttribute("value",e)}}i(x,"formAssociated",!0),i(x,"observed",["label","placeholder","type","value","name","hint","error","disabled","required"]),i(x,"styles",`
    :host { display: block; }
    .field { display: flex; flex-direction: column; gap: 6px; }
    label {
      font-size: var(--ga-fs-sm, 14px);
      font-weight: 500;
      color: var(--ga-fg, #ededed);
    }
    .req { color: var(--ga-red, #ff6568); margin-left: 2px; }
    input {
      width: 100%;
      font-family: inherit;
      font-size: var(--ga-fs-sm, 14px);
      color: var(--ga-fg, #ededed);
      background: var(--ga-bg-elev, #1a1a1a);
      border: 1px solid var(--ga-border-strong, #2a2a2a);
      border-radius: var(--ga-radius, 6px);
      padding: 10px 12px;
      transition: border-color var(--ga-transition, 0.18s ease),
        box-shadow var(--ga-transition, 0.18s ease);
    }
    input::placeholder { color: var(--ga-dim, #454545); }
    input:hover { border-color: var(--ga-muted, #878787); }
    input:focus {
      outline: none;
      border-color: var(--ga-accent, #54a2ff);
      box-shadow: 0 0 0 3px color-mix(in srgb, var(--ga-accent, #54a2ff) 25%, transparent);
    }
    :host([disabled]) input { opacity: 0.5; cursor: not-allowed; }
    .hint { font-size: var(--ga-fs-xs, 12px); color: var(--ga-muted, #878787); }
    .error { font-size: var(--ga-fs-xs, 12px); color: var(--ga-red, #ff6568); }
    :host([error]) input { border-color: var(--ga-red, #ff6568); }
  `);c("ga-input",x);class m extends d{template(){const e=this.hasFlag("checked"),t=this.attr("label");return`
      <label class="wrap">
        <button
          part="track"
          type="button"
          role="switch"
          aria-checked="${e}"
          ${this.hasFlag("disabled")?"disabled":""}
        ><span class="knob" part="knob"></span></button>
        ${t?`<span class="label">${n(t)}</span>`:""}
      </label>
    `}render(){var e;super.render(),(e=this.$("button"))==null||e.addEventListener("click",()=>this.toggle())}toggle(){if(this.hasFlag("disabled"))return;const e=!this.hasFlag("checked");this.toggleAttribute("checked",e),this.emit("change",{checked:e})}get checked(){return this.hasFlag("checked")}set checked(e){this.toggleAttribute("checked",!!e)}}i(m,"formAssociated",!0),i(m,"observed",["checked","disabled","label"]),i(m,"styles",`
    :host { display: inline-block; }
    .wrap {
      display: inline-flex;
      align-items: center;
      gap: var(--ga-space-3, 12px);
      cursor: pointer;
      user-select: none;
    }
    :host([disabled]) .wrap { opacity: 0.5; cursor: not-allowed; }
    button {
      position: relative;
      flex: none;
      width: 40px; height: 24px;
      padding: 0;
      border: 1px solid var(--ga-border-strong, #2a2a2a);
      border-radius: var(--ga-radius-full, 9999px);
      background: var(--ga-bg-elev, #1a1a1a);
      cursor: inherit;
      transition: background var(--ga-transition, 0.18s ease),
        border-color var(--ga-transition, 0.18s ease);
    }
    button:focus-visible {
      outline: none;
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
    }
    .knob {
      position: absolute;
      top: 2px; left: 2px;
      width: 18px; height: 18px;
      border-radius: 50%;
      background: var(--ga-muted, #878787);
      transition: transform var(--ga-transition, 0.18s ease),
        background var(--ga-transition, 0.18s ease);
    }
    :host([checked]) button {
      background: var(--ga-accent, #54a2ff);
      border-color: var(--ga-accent, #54a2ff);
    }
    :host([checked]) .knob {
      transform: translateX(16px);
      background: var(--ga-accent-contrast, #000);
    }
    .label { font-size: var(--ga-fs-sm, 14px); color: var(--ga-fg, #ededed); }
  `);c("ga-switch",m);class I extends d{template(){return'<div class="spinner" part="spinner" role="status" aria-label="Loading"></div>'}}i(I,"observed",["size","color"]),i(I,"styles",`
    :host { display: inline-flex; }
    .spinner {
      width: 20px; height: 20px;
      border: 2px solid color-mix(in srgb, currentColor 25%, transparent);
      border-top-color: currentColor;
      border-radius: 50%;
      color: var(--ga-accent, #54a2ff);
      animation: spin 0.7s linear infinite;
    }
    :host([size="sm"]) .spinner { width: 14px; height: 14px; }
    :host([size="lg"]) .spinner { width: 32px; height: 32px; border-width: 3px; }
    :host([color="green"])  .spinner { color: var(--ga-green, #00c758); }
    :host([color="amber"])  .spinner { color: var(--ga-amber, #fcbb00); }
    :host([color="purple"]) .spinner { color: var(--ga-purple, #ac4bff); }
    :host([color="red"])    .spinner { color: var(--ga-red, #ff6568); }
    :host([color="fg"])     .spinner { color: var(--ga-fg, #ededed); }
    @keyframes spin { to { transform: rotate(360deg); } }
  `);c("ga-spinner",I);class B extends d{template(){const e=this.attr("title"),t=this.hasFlag("dismissible")?'<button class="close" part="close" aria-label="Dismiss">&times;</button>':"";return`
      <div class="alert" part="alert" role="alert">
        <span class="dot" aria-hidden="true"></span>
        <div class="content">
          ${e?`<div class="title" part="title">${n(e)}</div>`:""}
          <slot></slot>
        </div>
        ${t}
      </div>
    `}render(){var e;super.render(),(e=this.$(".close"))==null||e.addEventListener("click",()=>{this.emit("dismiss"),this.remove()})}}i(B,"observed",["tone","title","dismissible"]),i(B,"styles",`
    :host { display: block; }
    .alert {
      --_c: var(--ga-muted, #878787);
      display: flex;
      gap: var(--ga-space-3, 12px);
      padding: var(--ga-space-4, 16px);
      border: 1px solid color-mix(in srgb, var(--_c) 35%, transparent);
      border-left-width: 3px;
      border-radius: var(--ga-radius, 6px);
      background: color-mix(in srgb, var(--_c) 8%, transparent);
      color: var(--ga-fg, #ededed);
      font-size: var(--ga-fs-sm, 14px);
      line-height: 1.5;
    }
    :host([tone="info"])    .alert { --_c: var(--ga-blue, #54a2ff); }
    :host([tone="success"]) .alert { --_c: var(--ga-green, #00c758); }
    :host([tone="warning"]) .alert { --_c: var(--ga-amber, #fcbb00); }
    :host([tone="danger"])  .alert { --_c: var(--ga-red, #ff6568); }

    .dot { flex: none; width: 8px; height: 8px; margin-top: 6px; border-radius: 50%; background: var(--_c); }
    .content { flex: 1; min-width: 0; }
    .title { font-weight: 600; color: var(--_c); margin-bottom: 2px; }
    .close {
      flex: none;
      background: none; border: none; cursor: pointer;
      color: var(--ga-muted, #878787);
      font-size: 18px; line-height: 1; padding: 0 4px;
      transition: color var(--ga-transition, 0.18s ease);
    }
    .close:hover { color: var(--ga-fg, #ededed); }
  `);c("ga-alert",B);class X extends d{template(){return'<kbd part="kbd"><slot></slot></kbd>'}}i(X,"styles",`
    :host { display: inline-block; }
    kbd {
      display: inline-block;
      font-family: var(--ga-font-mono, ui-monospace, monospace);
      font-size: var(--ga-fs-xs, 12px);
      line-height: 1;
      color: var(--ga-muted, #878787);
      background: var(--ga-bg-elev, #1a1a1a);
      border: 1px solid var(--ga-border-strong, #2a2a2a);
      border-bottom-width: 2px;
      border-radius: var(--ga-radius, 6px);
      padding: 4px 7px;
      min-width: 1em;
      text-align: center;
    }
  `);c("ga-kbd",X);class D extends d{_parse(){try{return JSON.parse(this.attr("tabs","[]"))}catch{return[]}}template(){var s;const e=this._parse(),t=this.attr("active")||((s=e[0])==null?void 0:s.id),a=e.map(l=>`
      <button class="tab" part="tab" role="tab" data-id="${n(l.id)}"
        aria-selected="${l.id===t}" tabindex="${l.id===t?"0":"-1"}">
        ${n(l.label)}
      </button>`).join(""),r=e.map(l=>`
      <div role="tabpanel" ${l.id===t?"":"hidden"}>
        <slot name="${n(l.id)}"></slot>
      </div>`).join("");return`
      <div class="list" part="list" role="tablist">${a}</div>
      <div class="panels" part="panels">${r}</div>
    `}render(){super.render(),this.shadowRoot.querySelectorAll(".tab").forEach(e=>{e.addEventListener("click",()=>this._select(e.dataset.id))})}_select(e){e!==this.attr("active")&&(this.setAttribute("active",e),this.emit("change",{id:e}))}}i(D,"observed",["tabs","active"]),i(D,"styles",`
    :host { display: block; }
    .list {
      display: flex;
      gap: var(--ga-space-1, 4px);
      border-bottom: 1px solid var(--ga-border, #1a1a1a);
    }
    .tab {
      position: relative;
      font-family: inherit;
      font-size: var(--ga-fs-sm, 14px);
      font-weight: 500;
      color: var(--ga-muted, #878787);
      background: none;
      border: none;
      padding: 10px 14px;
      cursor: pointer;
      transition: color var(--ga-transition, 0.18s ease);
    }
    .tab:hover { color: var(--ga-fg, #ededed); }
    .tab[aria-selected="true"] { color: var(--ga-fg, #ededed); }
    .tab[aria-selected="true"]::after {
      content: "";
      position: absolute;
      left: 8px; right: 8px; bottom: -1px;
      height: 2px;
      background: var(--ga-accent, #54a2ff);
      border-radius: 2px;
    }
    .tab:focus-visible {
      outline: none;
      box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff);
      border-radius: var(--ga-radius, 6px);
    }
    .panels { padding-top: var(--ga-space-4, 16px); }
  `);c("ga-tabs",D);class H extends d{template(){const e=this.attr("title");return`
      <div class="note" part="note">
        ${e?`<div class="title" part="title">${n(e)}</div>`:""}
        <slot></slot>
      </div>
    `}}i(H,"observed",["tone","title"]),i(H,"styles",`
    :host { display: block; }
    .note {
      --_c: var(--ga-accent, #54a2ff);
      margin: 0;
      padding: 14px 16px;
      background: var(--ga-bg-elev, #1a1a1a);
      border: 1px solid var(--ga-border, #1a1a1a);
      border-left: 3px solid var(--_c);
      border-radius: var(--ga-radius, 6px);
      font-size: 15px;
      line-height: 1.55;
      color: var(--ga-muted, #878787);
    }
    :host([tone="neutral"]) .note { --_c: var(--ga-dim, #454545); }
    :host([tone="info"])    .note { --_c: var(--ga-blue, #54a2ff); }
    :host([tone="success"]) .note { --_c: var(--ga-green, #00c758); }
    :host([tone="warning"]) .note { --_c: var(--ga-amber, #fcbb00); }
    :host([tone="error"])   .note,
    :host([tone="danger"])  .note { --_c: var(--ga-red, #ff6568); }

    .title { margin: 0 0 3px; font-size: 14px; font-weight: 600; color: var(--ga-fg, #ededed); }
  `);c("ga-note",H);const se={compass:'<circle cx="12" cy="12" r="10"/><polygon points="16.24 7.76 14.12 14.12 7.76 16.24 9.88 9.88 16.24 7.76"/>',bookmark:'<path d="M19 21l-7-5-7 5V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2z"/>',star:'<polygon points="12 2 15.09 8.26 22 9.27 17 14.14 18.18 21.02 12 17.77 5.82 21.02 7 14.14 2 9.27 8.91 8.26 12 2"/>',heart:'<path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"/>',plus:'<line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>',minus:'<line x1="5" y1="12" x2="19" y2="12"/>',check:'<polyline points="20 6 9 17 4 12"/>',x:'<line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>',trash:'<polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>',bell:'<path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/>',user:'<path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>',home:'<path d="M3 9l9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/><polyline points="9 22 9 12 15 12 15 22"/>',search:'<circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>',settings:'<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>',menu:'<line x1="3" y1="12" x2="21" y2="12"/><line x1="3" y1="6" x2="21" y2="6"/><line x1="3" y1="18" x2="21" y2="18"/>',"chevron-right":'<polyline points="9 18 15 12 9 6"/>',"chevron-down":'<polyline points="6 9 12 15 18 9"/>',"arrow-right":'<line x1="5" y1="12" x2="19" y2="12"/><polyline points="12 5 19 12 12 19"/>',"external-link":'<path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/>',upload:'<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/>',download:'<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/>',image:'<rect x="3" y="3" width="18" height="18" rx="2" ry="2"/><circle cx="8.5" cy="8.5" r="1.5"/><polyline points="21 15 16 10 5 21"/>',info:'<circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/>',layers:'<polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/>',sun:'<circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/>',moon:'<path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/>',github:'<path d="M9 19c-5 1.5-5-2.5-7-3m14 6v-3.87a3.37 3.37 0 0 0-.94-2.61c3.14-.35 6.44-1.54 6.44-7A5.44 5.44 0 0 0 20 4.77 5.07 5.07 0 0 0 19.91 1S18.73.65 16 2.48a13.38 13.38 0 0 0-7 0C6.27.65 5.09 1 5.09 1A5.07 5.07 0 0 0 5 4.77a5.44 5.44 0 0 0-1.5 3.78c0 5.42 3.3 6.61 6.44 7A3.37 3.37 0 0 0 9 18.13V22"/>'};class N extends d{template(){const e=se[this.attr("name")]||"",t=Number(this.attr("size"))||20;return`<svg viewBox="0 0 24 24" width="${t}" height="${t}" part="svg" aria-hidden="true">${e}</svg>`}}i(N,"observed",["name","size"]),i(N,"styles",`
    :host { display: inline-flex; line-height: 0; }
    svg {
      display: block;
      stroke: currentColor; fill: none;
      stroke-width: 2; stroke-linecap: round; stroke-linejoin: round;
    }
  `);c("ga-icon",N);class R extends d{template(){const e=this.attr("label","Drop files here or click to browse");return`
      <label class="drop" part="drop">
        <ga-icon class="icon" name="upload" size="24"></ga-icon>
        <span class="label">${n(e)}</span>
        <span class="hint"><slot></slot></span>
        <input type="file" ${this.hasFlag("multiple")?"multiple":""} accept="${n(this.attr("accept"))}" />
      </label>
    `}render(){super.render();const e=this.$(".drop"),t=this.$("input");t.addEventListener("change",()=>this._emit(t.files)),["dragenter","dragover"].forEach(a=>e.addEventListener(a,r=>{r.preventDefault(),e.classList.add("dragging")})),["dragleave","dragend","drop"].forEach(a=>e.addEventListener(a,r=>{r.preventDefault(),e.classList.remove("dragging")})),e.addEventListener("drop",a=>{var r,s;(s=(r=a.dataTransfer)==null?void 0:r.files)!=null&&s.length&&this._emit(a.dataTransfer.files)})}_emit(e){e&&e.length&&this.emit("files",{files:Array.from(e)})}}i(R,"observed",["accept","multiple","label"]),i(R,"styles",`
    :host { display: block; }
    .drop {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 8px;
      border: 1px dashed var(--ga-border-strong, #2a2a2a);
      border-radius: var(--ga-radius, 6px);
      padding: 34px 18px;
      text-align: center;
      color: var(--ga-muted, #878787);
      cursor: pointer;
      background: var(--ga-bg-elev, #1a1a1a);
      transition: border-color 0.15s, background 0.15s, color 0.15s;
    }
    .drop:hover { background: var(--ga-bg-elev-hover, #1f1f1f); }
    .drop.dragging {
      border-color: var(--ga-accent, #54a2ff);
      color: var(--ga-fg, #ededed);
      background: color-mix(in srgb, var(--ga-accent, #54a2ff) 8%, transparent);
    }
    .icon { opacity: 0.85; }
    .label { font-size: var(--ga-fs-sm, 14px); }
    .hint { font-size: var(--ga-fs-xs, 12px); color: var(--ga-dim, #454545); }
    .hint:empty { display: none; }
    input { display: none; }
  `);c("ga-file-drop",R);class Y extends d{template(){return`
      <button class="fab" part="fab" aria-label="${n(this.attr("label","Action"))}">
        <slot>+</slot>
      </button>
    `}}i(Y,"observed",["color","position","label"]),i(Y,"styles",`
    :host { display: contents; }
    .fab {
      --_c: var(--ga-accent, #54a2ff);
      position: fixed;
      right: max(20px, env(safe-area-inset-right));
      bottom: max(20px, env(safe-area-inset-bottom));
      z-index: 40;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 56px;
      height: 56px;
      padding: 0;
      font-size: 22px;
      line-height: 1;
      background: var(--_c);
      color: var(--ga-accent-contrast, #000);
      border: 0;
      border-radius: 50%;
      box-shadow: 0 8px 24px rgba(0, 0, 0, 0.35);
      cursor: pointer;
      transition: filter 0.15s ease, transform 0.15s ease;
    }
    .fab:hover { filter: brightness(1.08); }
    .fab:active { transform: translateY(1px); }
    .fab:focus-visible { outline: 2px solid var(--ga-fg, #ededed); outline-offset: 3px; }

    :host([position="bottom-left"]) .fab { left: max(20px, env(safe-area-inset-left)); right: auto; }
    :host([position="static"]) .fab { position: static; }

    :host([color="green"])  .fab { --_c: var(--ga-green, #00c758); }
    :host([color="amber"])  .fab { --_c: var(--ga-amber, #fcbb00); }
    :host([color="purple"]) .fab { --_c: var(--ga-purple, #ac4bff); }
    :host([color="red"])    .fab { --_c: var(--ga-red, #ff6568); }
  `);c("ga-fab",Y);class V extends d{template(){const e=this.attr("title");return`
      <div class="scrim" part="scrim"></div>
      <div class="panel" part="panel" role="dialog" aria-modal="true">
        <div class="head" part="header">
          <span class="title"><slot name="header">${n(e)}</slot></span>
          <button class="close" aria-label="Close">&times;</button>
        </div>
        <div class="body" part="body"><slot></slot></div>
        <div class="foot" part="footer"><slot name="footer"></slot></div>
      </div>
    `}connectedCallback(){super.connectedCallback(),this._key=e=>{e.key==="Escape"&&this.open&&this.close()},document.addEventListener("keydown",this._key),this.shadowRoot.addEventListener("slotchange",()=>this._syncFooter())}disconnectedCallback(){this._key&&document.removeEventListener("keydown",this._key)}render(){var e,t;super.render(),(e=this.$(".close"))==null||e.addEventListener("click",()=>this.close()),(t=this.$(".scrim"))==null||t.addEventListener("click",()=>this.close()),this._syncFooter()}_syncFooter(){const e=this.$('slot[name="footer"]'),t=this.$(".foot");e&&t&&t.classList.toggle("show",e.assignedNodes().length>0)}get open(){return this.hasFlag("open")}set open(e){this.toggleAttribute("open",!!e)}show(){this.open||(this.setAttribute("open",""),this.emit("open"))}close(){this.open&&(this.removeAttribute("open"),this.emit("close"))}toggle(){this.open?this.close():this.show()}}i(V,"observed",["open","side","title"]),i(V,"styles",`
    :host { display: contents; }
    .scrim {
      position: fixed; inset: 0; z-index: 49;
      background: rgba(0, 0, 0, 0.5);
      opacity: 0; visibility: hidden;
      transition: opacity 0.32s ease, visibility 0.32s;
    }
    :host([open]) .scrim { opacity: 1; visibility: visible; }

    .panel {
      position: fixed; top: 0; right: 0; z-index: 50;
      width: min(420px, 100%); height: 100%;
      display: flex; flex-direction: column;
      background: var(--ga-bg, #000);
      border-left: 1px solid var(--ga-border, #1a1a1a);
      box-shadow: -16px 0 40px rgba(0, 0, 0, 0.4);
      transform: translateX(100%);
      transition: transform 0.32s cubic-bezier(0.4, 0, 0.2, 1);
      visibility: hidden;
    }
    :host([side="left"]) .panel {
      right: auto; left: 0;
      border-left: 0; border-right: 1px solid var(--ga-border, #1a1a1a);
      box-shadow: 16px 0 40px rgba(0, 0, 0, 0.4);
      transform: translateX(-100%);
    }
    :host([open]) .panel { transform: translateX(0); visibility: visible; }

    .head {
      display: flex; align-items: center; justify-content: space-between; gap: 12px;
      padding: 18px 20px; border-bottom: 1px solid var(--ga-border, #1a1a1a);
      font-weight: 600; color: var(--ga-fg, #ededed);
    }
    .body { flex: 1; overflow: auto; padding: 20px; color: var(--ga-muted, #878787); line-height: 1.55; }
    .foot { padding: 16px 20px; border-top: 1px solid var(--ga-border, #1a1a1a); }
    .foot { display: none; }
    .foot.show { display: block; }
    .close {
      flex: none; background: none; border: 0; cursor: pointer;
      color: var(--ga-muted, #878787); font-size: 22px; line-height: 1; padding: 2px 6px;
      transition: color var(--ga-transition, 0.18s ease);
    }
    .close:hover { color: var(--ga-fg, #ededed); }
  `);c("ga-panel",V);class y extends d{constructor(){var e;super(),this._internals=(e=this.attachInternals)==null?void 0:e.call(this)}template(){const e=this.attr("label"),t=this.attr("value","50");return`
      <div class="wrap">
        ${e?`<div class="top"><span class="label">${n(e)}</span><span class="val">${n(t)}</span></div>`:""}
        <input type="range"
          min="${n(this.attr("min","0"))}"
          max="${n(this.attr("max","100"))}"
          step="${n(this.attr("step","1"))}"
          value="${n(t)}"
          ${this.hasFlag("disabled")?"disabled":""} />
      </div>
    `}render(){var a;super.render();const e=this.$("input"),t=this.$(".val");e&&((a=this._internals)==null||a.setFormValue(e.value),e.addEventListener("input",()=>{var r;this._value=e.value,t&&(t.textContent=e.value),(r=this._internals)==null||r.setFormValue(e.value),this.emit("input",{value:e.value})}),e.addEventListener("change",()=>this.emit("change",{value:e.value})))}get value(){var e;return((e=this.$("input"))==null?void 0:e.value)??this._value??this.attr("value")}set value(e){this._value=e,this.setAttribute("value",e)}}i(y,"formAssociated",!0),i(y,"observed",["min","max","step","value","label","disabled"]),i(y,"styles",`
    :host { display: block; }
    .wrap { display: flex; flex-direction: column; gap: 8px; }
    :host([disabled]) .wrap { opacity: 0.5; pointer-events: none; }
    .top { display: flex; align-items: baseline; justify-content: space-between; }
    .label { font-size: var(--ga-fs-sm, 14px); font-weight: 500; color: var(--ga-fg, #ededed); }
    .val { font-family: var(--ga-font-mono, ui-monospace, monospace); font-size: var(--ga-fs-sm, 14px); color: var(--ga-muted, #878787); }

    input[type="range"] {
      -webkit-appearance: none; appearance: none;
      width: 100%; height: 6px; margin: 6px 0;
      border-radius: var(--ga-radius-full, 9999px);
      background: var(--ga-bg-elev-hover, #1f1f1f);
      accent-color: var(--ga-accent, #54a2ff);
      cursor: pointer; outline: none;
    }
    input[type="range"]:focus-visible { box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff); }
    input[type="range"]::-webkit-slider-thumb {
      -webkit-appearance: none; appearance: none;
      width: 18px; height: 18px; border-radius: 50%;
      background: var(--ga-accent, #54a2ff);
      border: 2px solid var(--ga-bg, #000);
      box-shadow: 0 1px 3px rgba(0, 0, 0, 0.4);
      cursor: pointer;
    }
    input[type="range"]::-moz-range-thumb {
      width: 16px; height: 16px; border: 2px solid var(--ga-bg, #000); border-radius: 50%;
      background: var(--ga-accent, #54a2ff); cursor: pointer;
    }
    input[type="range"]::-moz-range-track { height: 6px; border-radius: 9999px; background: var(--ga-bg-elev-hover, #1f1f1f); }
  `);c("ga-slider",y);class P extends d{template(){const e=this.attr("href"),t=e?"a":"div",a=e?`href="${n(e)}"`:"";return`
      <header class="hdr" part="header">
        <${t} class="brand" part="brand" ${a}><slot name="brand">${n(this.attr("brand"))}</slot></${t}>
        <div class="spacer"></div>
        <nav class="actions" part="actions"><slot></slot></nav>
      </header>
    `}}i(P,"observed",["brand","href","static"]),i(P,"styles",`
    :host { display: block; }
    .hdr {
      position: sticky; top: 0; z-index: 50;
      display: flex; align-items: center; gap: 16px;
      height: 56px; padding: 0 16px;
      background: var(--ga-bg, #000);
    }
    :host([static]) .hdr { position: static; }

    .brand {
      flex: 0 1 auto; min-width: 0;
      font-size: var(--ga-fs-base, 17px); font-weight: 600;
      color: var(--ga-fg, #ededed); text-decoration: none;
      white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
      transition: color var(--ga-transition, 0.18s ease);
    }
    .brand:hover { color: var(--ga-muted, #878787); }

    .spacer { flex: 1 1 auto; }

    .actions { flex: none; display: flex; align-items: center; gap: 16px; }
    /* Slotted links live in the light DOM, so the host page's own \`a\` rules
       would otherwise win (outer tree beats ::slotted on the cascade). Use
       !important to keep the opinionated muted-nav look; consumers can still
       override with their own !important or by targeting ::part. */
    ::slotted(a) {
      color: var(--ga-muted, #878787) !important; text-decoration: none !important;
      font-size: var(--ga-fs-sm, 14px);
      transition: color var(--ga-transition, 0.18s ease);
    }
    ::slotted(a:hover) { color: var(--ga-fg, #ededed) !important; }
  `);c("ga-header",P);class U extends d{template(){return`
      <div class="sheet" part="sheet">
        <div class="grip" part="handle"><span class="bar"></span></div>
        <div class="head" part="header"><slot name="header"></slot></div>
        <div class="body" part="body"><slot></slot></div>
      </div>
    `}connectedCallback(){super.connectedCallback(),this._onResize=()=>this._apply(),window.addEventListener("resize",this._onResize),this._onMove=e=>this._move(e),this._onUp=()=>this._up(),window.addEventListener("pointermove",this._onMove),window.addEventListener("pointerup",this._onUp)}disconnectedCallback(){window.removeEventListener("resize",this._onResize),window.removeEventListener("pointermove",this._onMove),window.removeEventListener("pointerup",this._onUp)}attributeChangedCallback(){this._mounted&&this._apply()}render(){super.render();const e=this.$(".grip"),t=this.$(".head");for(const a of[e,t])a==null||a.addEventListener("pointerdown",r=>this._down(r));requestAnimationFrame(()=>this._apply())}get open(){return this.hasFlag("open")}get snap(){return this.attr("snap","half")}show(e){e&&this.setAttribute("snap",e),this.setAttribute("open",""),this._apply(),this.emit("open")}close(){this.removeAttribute("open"),this._apply(),this.emit("close")}snapTo(e){this.setAttribute("snap",e),this._apply(),this.emit("snapchange",{snap:e})}_snaps(){var a;const e=((a=this.$(".sheet"))==null?void 0:a.offsetHeight)||window.innerHeight*.88,t=window.innerHeight;return{full:0,half:Math.max(0,e-t*.45),peek:Math.max(0,e-128),closed:e}}_currentY(){var t;const e=/translateY\(([-0-9.]+)px\)/.exec(((t=this.$(".sheet"))==null?void 0:t.style.transform)||"");return e?parseFloat(e[1]):this._snaps().closed}_apply(){const e=this.$(".sheet");if(!e)return;const t=this._snaps(),a=this.open?t[this.snap]??t.half:t.closed;e.style.transform=`translateY(${a}px)`}_down(e){var t;this._dragging=!0,this._startY=e.clientY,this._startTf=this._currentY(),(t=this.$(".sheet"))==null||t.classList.add("dragging")}_move(e){if(!this._dragging)return;const t=this._snaps(),a=Math.min(t.closed,Math.max(0,this._startTf+(e.clientY-this._startY)));this.$(".sheet").style.transform=`translateY(${a}px)`}_up(){var r;if(!this._dragging)return;this._dragging=!1,(r=this.$(".sheet"))==null||r.classList.remove("dragging");const e=this._snaps(),t=this._currentY();if(t>e.peek+80){this.close();return}let a="full";for(const s of["full","half","peek"])Math.abs(e[s]-t)<Math.abs(e[a]-t)&&(a=s);a!==this.snap&&(this.setAttribute("snap",a),this.emit("snapchange",{snap:a})),this._apply()}}i(U,"observed",["open","snap"]),i(U,"styles",`
    :host { display: contents; }
    .sheet {
      position: fixed; left: 0; right: 0; bottom: 0; z-index: 50;
      width: min(560px, 100%); height: 88vh; margin: 0 auto;
      display: flex; flex-direction: column;
      background: var(--ga-bg, #000);
      border: 1px solid var(--ga-border, #1a1a1a); border-bottom: 0;
      border-radius: 16px 16px 0 0;
      box-shadow: 0 -16px 40px rgba(0, 0, 0, 0.4);
      transform: translateY(100%);
      transition: transform 0.32s cubic-bezier(0.4, 0, 0.2, 1);
      touch-action: none;
    }
    .sheet.dragging { transition: none; }

    .grip { flex: none; display: flex; justify-content: center; padding: 10px 0 6px; cursor: grab; }
    .grip:active { cursor: grabbing; }
    .bar { width: 40px; height: 5px; border-radius: 9999px; background: var(--ga-border-strong, #2a2a2a); }

    .head { flex: none; padding: 4px 20px 12px; color: var(--ga-fg, #ededed); cursor: grab; }
    .head:active { cursor: grabbing; }
    .head:empty { display: none; }

    .body { flex: 1; overflow-y: auto; padding: 0 20px 24px; color: var(--ga-muted, #878787); line-height: 1.55; }
  `);c("ga-bottom-sheet",U);class J extends d{_parse(){try{return JSON.parse(this.attr("items","[]"))}catch{return[]}}template(){var r;const e=this._parse(),t=this.attr("active")||((r=e[0])==null?void 0:r.id);return`<nav class="nav" part="nav" role="navigation">${e.map(s=>{const l=s.icon||"",p=/^[a-z][a-z0-9-]*$/.test(l)?`<ga-icon class="icon" name="${n(l)}" size="22"></ga-icon>`:`<span class="icon" aria-hidden="true">${n(l||"•")}</span>`;return`
      <button class="item" part="item" data-id="${n(s.id)}"
        ${s.id===t?'aria-current="page"':""}>
        ${p}
        <span class="label">${n(s.label)}</span>
      </button>`}).join("")}</nav>`}render(){super.render(),this.shadowRoot.querySelectorAll(".item").forEach(e=>e.addEventListener("click",()=>this._select(e.dataset.id)))}_select(e){e!==this.attr("active")&&(this.setAttribute("active",e),this.emit("change",{id:e}))}}i(J,"observed",["items","active"]),i(J,"styles",`
    :host { display: block; }
    .nav {
      position: fixed; left: 0; right: 0; bottom: 0; z-index: 40;
      display: flex;
      background: var(--ga-bg, #000);
      border-top: 1px solid var(--ga-border, #1a1a1a);
      padding-bottom: env(safe-area-inset-bottom);
    }
    :host([static]) .nav {
      position: static;
      border: 1px solid var(--ga-border, #1a1a1a);
      border-radius: var(--ga-radius, 6px);
      padding-bottom: 0;
    }
    .item {
      flex: 1; min-width: 0;
      display: flex; flex-direction: column; align-items: center; gap: 3px;
      padding: 9px 4px 8px;
      background: none; border: 0; cursor: pointer;
      color: var(--ga-muted, #878787); font-family: inherit;
      transition: color var(--ga-transition, 0.18s ease);
    }
    .item:hover { color: var(--ga-fg, #ededed); }
    .item[aria-current="page"] { color: var(--ga-accent, #54a2ff); }
    .item:focus-visible { outline: none; box-shadow: var(--ga-ring, 0 0 0 2px #000, 0 0 0 4px #54a2ff); border-radius: var(--ga-radius, 6px); }
    .icon { font-size: 20px; line-height: 1; }
    .label { font-size: 11px; line-height: 1; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 100%; }
  `);c("ga-bottom-nav",J);const w=document.getElementById("sdl"),k=document.getElementById("sdl2"),_=document.getElementById("gql"),$=document.getElementById("vars"),W=document.getElementById("run"),u=document.getElementById("status"),E=document.getElementById("deep"),z=document.getElementById("isdl"),C=document.getElementById("iquery"),oe=document.getElementById("migration"),ie=document.getElementById("sql"),ne=document.getElementById("params"),le=document.getElementById("delta"),A=document.getElementById("deepout"),de=document.getElementById("maxdepth"),ce=document.getElementById("isql"),pe=document.getElementById("igraph");function ge(o){return new Promise((e,t)=>{const a=document.createElement("script");a.src=o,a.onload=()=>e(),a.onerror=()=>t(new Error(`failed to load ${o}`)),document.head.appendChild(a)})}function g(o,e){o.textContent=e}function he(o){const e=o.indexOf("CREATE PROPERTY GRAPH");if(e<0)return o;const t=o.indexOf(`
-- +goose Down`,e);return(t<0?o.slice(e):o.slice(e,t)).trim()}let h=!0;function K(){h=!0;const o=w.value,e=_.value,t=$.value,a=k.value,r=globalThis.gopgqlMigration(o);g(oe,r.error||r.migration),r.error&&(h=!1);const s=globalThis.gopgqlCompile(o,e,t);g(ie,s.error||s.sql),g(ne,s.error?"—":s.params),s.error&&(h=!1);const l=globalThis.gopgqlDelta(o,a);g(le,l.error||l.delta),l.error&&(h=!1);const p=globalThis.gopgqlCompile(o,E.value,t);p.depthExceeded?g(A,`rejected at compile time — *compiler.DepthExceededError

${p.error}

MaxDepth = ${p.maxDepth}. No SQL was emitted, so nothing reached a database.`):p.error?(g(A,p.error),h=!1):g(A,`compiled within MaxDepth ${p.maxDepth}:

${p.sql}`);const b=z.value,v=globalThis.gopgqlCompile(b,C.value,"");g(ce,v.error||v.sql),v.error&&(h=!1);const L=globalThis.gopgqlMigration(b);g(pe,L.error||he(L.migration)),L.error&&(h=!1),h?(u.textContent="generated",u.className="status ok"):(u.textContent="see errors below",u.className="status error")}async function ue(){try{await ge("wasm_exec.js");const o=new globalThis.Go,e=await fetch("gopgql.wasm");if(!e.ok)throw new Error(`gopgql.wasm: HTTP ${e.status}`);const t=await e.arrayBuffer(),{instance:a}=await WebAssembly.instantiate(t,o.importObject);o.run(a),w.value=globalThis.gopgqlExampleSDL||w.value,_.value=globalThis.gopgqlExampleQuery||_.value,$.value=globalThis.gopgqlExampleVars||$.value,k.value=globalThis.gopgqlRevisedExampleSDL||k.value,E.value=globalThis.gopgqlExampleDeepQuery||E.value,z.value=globalThis.gopgqlExampleInterfaceSDL||z.value,C.value=globalThis.gopgqlExampleInterfaceQuery||C.value,de.textContent=String(globalThis.gopgqlMaxDepth??3),W.removeAttribute("disabled"),u.textContent="ready",u.className="status ok",K()}catch(o){u.textContent=String(o),u.className="status error"}}W.addEventListener("click",K);for(const o of[w,_,$,k,E,z,C])o.addEventListener("input",()=>{globalThis.gopgqlMigration&&K()});ue();
