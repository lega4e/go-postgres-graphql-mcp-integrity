/* =========================================================================
   Public type entry for `@lega4e/ui-kit`.

   Re-exports every component class and pulls in the ambient DOM augmentation
   (`HTMLElementTagNameMap`) so `import "@lega4e/ui-kit"` gives vanilla /
   Vue / Svelte / Solid users typed `document.querySelector("ga-card")` etc.

   React's JSX is intentionally NOT augmented here — see the separate, opt-in
   `@lega4e/ui-kit/react` entry.

   The per-component `.d.ts` files are generated from the JSDoc via
   `npm run types` (tsc --allowJs --declaration --emitDeclarationOnly).
   ========================================================================= */

/// <reference path="./global.d.ts" />

export { GaElement, define, esc } from "./core/base-element.js";

export { GaButton } from "./components/button/button.js";
export { GaRadioGroup } from "./components/radio-group/radio-group.js";
export { GaCode } from "./components/code/code.js";
export { GaBreadcrumbs } from "./components/breadcrumbs/breadcrumbs.js";
export { GaTable } from "./components/table/table.js";
export { GaBadge } from "./components/badge/badge.js";
export { GaCard } from "./components/card/card.js";
export { GaAvatar } from "./components/avatar/avatar.js";
export { GaInput } from "./components/input/input.js";
export { GaSwitch } from "./components/switch/switch.js";
export { GaSpinner } from "./components/spinner/spinner.js";
export { GaAlert } from "./components/alert/alert.js";
export { GaKbd } from "./components/kbd/kbd.js";
export { GaTabs } from "./components/tabs/tabs.js";
export { GaNote } from "./components/note/note.js";
export { GaFileDrop } from "./components/file-drop/file-drop.js";
export { GaFab } from "./components/fab/fab.js";
export { GaPanel } from "./components/panel/panel.js";
export { GaSlider } from "./components/slider/slider.js";
export { GaHeader } from "./components/header/header.js";
export { GaBottomSheet } from "./components/bottom-sheet/bottom-sheet.js";
export { GaBottomNav } from "./components/bottom-nav/bottom-nav.js";
export { GaIcon } from "./components/icon/icon.js";
