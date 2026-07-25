/* =========================================================================
   CodeMirror 6 editors for the playground.

   Every code box on the page — the SDL and query inputs, the variables, and
   the generated schema/SQL/migration outputs — is a real editor rather than a
   <textarea> or a block of plain text, so GraphQL, JSON and SQL are all
   highlighted with the same house palette.

   The SQL side is the interesting one. PostgreSQL 19's SQL/PGQ syntax
   (GRAPH_TABLE, MATCH, COLUMNS, CREATE PROPERTY GRAPH …) is too new for any
   published grammar, so instead of writing one, the stock PostgreSQL dialect
   is *extended* with those keywords — `@codemirror/lang-sql` exposes the spec
   a dialect was defined from, so the new words are a one-line addition rather
   than a fork.
   ========================================================================= */

import { EditorState } from '@codemirror/state'
import { EditorView, keymap, highlightSpecialChars, drawSelection } from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import {
  HighlightStyle, bracketMatching, indentOnInput, syntaxHighlighting,
} from '@codemirror/language'
import { tags as t } from '@lezer/highlight'
import { sql, SQLDialect, PostgreSQL } from '@codemirror/lang-sql'
import { json } from '@codemirror/lang-json'
import { graphql } from 'cm6-graphql'

/**
 * The SQL/PGQ words PostgreSQL 19 adds. `MATCH`, `SOURCE`, `KEY` and a few
 * others are already SQL keywords; the rest are what a stock PostgreSQL
 * highlighter renders as plain identifiers today.
 */
const PGQ_KEYWORDS =
  'graph_table graph property vertex edge tables label properties ' +
  'destination is are cheapest walk trail acyclic simple element'

/** PostgreSQL, plus the graph vocabulary — the dialect the SQL panes use. */
const PostgreSQL19 = SQLDialect.define({
  ...PostgreSQL.spec,
  keywords: PostgreSQL.spec.keywords + ' ' + PGQ_KEYWORDS,
})

/**
 * Token colours, expressed in the kit's own tokens so the editors follow the
 * page theme instead of carrying a palette of their own.
 */
const houseHighlight = HighlightStyle.define([
  { tag: [t.keyword, t.moduleKeyword, t.operatorKeyword], color: 'var(--ga-purple, #ac4bff)' },
  { tag: [t.string, t.special(t.string)], color: 'var(--ga-green, #00c758)' },
  { tag: [t.number, t.bool, t.null], color: 'var(--ga-amber, #fcbb00)' },
  { tag: [t.comment, t.lineComment, t.blockComment], color: 'var(--ga-muted, #878787)', fontStyle: 'italic' },
  { tag: [t.typeName, t.className, t.namespace], color: 'var(--ga-blue, #54a2ff)' },
  { tag: [t.propertyName, t.attributeName], color: 'var(--ga-fg, #ededed)' },
  { tag: [t.variableName, t.definition(t.variableName)], color: 'var(--ga-fg, #ededed)' },
  { tag: [t.function(t.variableName), t.labelName], color: 'var(--ga-blue, #54a2ff)' },
  { tag: [t.operator, t.punctuation, t.separator, t.bracket], color: 'var(--ga-muted, #878787)' },
  { tag: t.invalid, color: 'var(--ga-red, #ff6568)' },
])

/** Chrome: the surface, border and focus ring the rest of the page uses. */
const houseTheme = EditorView.theme({
  '&': {
    background: 'var(--ga-bg-elev, #1a1a1a)',
    border: '1px solid var(--ga-border-strong, #2a2a2a)',
    borderRadius: 'var(--ga-radius, 6px)',
    color: 'var(--ga-fg, #ededed)',
    fontSize: 'var(--ga-fs-sm, 14px)',
  },
  '&.cm-focused': {
    outline: 'none',
    borderColor: 'var(--ga-accent, #54a2ff)',
  },
  '.cm-content': {
    fontFamily: 'var(--ga-font-mono, ui-monospace, monospace)',
    padding: '10px 0',
    caretColor: 'var(--ga-accent, #54a2ff)',
  },
  '.cm-line': { padding: '0 12px' },
  '.cm-scroller': { overflow: 'auto', lineHeight: '1.5' },
  '.cm-cursor': { borderLeftColor: 'var(--ga-accent, #54a2ff)' },
  '&.cm-editor .cm-selectionBackground, ::selection': {
    background: 'color-mix(in srgb, var(--ga-accent, #54a2ff) 28%, transparent)',
  },
  '.cm-matchingBracket': {
    background: 'color-mix(in srgb, var(--ga-accent, #54a2ff) 22%, transparent)',
    outline: 'none',
  },
}, { dark: true })

/** The language extension for a pane, chosen by its `data-lang`. */
function language(lang) {
  switch (lang) {
    case 'graphql':
      return graphql()
    case 'json':
      return json()
    case 'sql':
    default:
      return sql({ dialect: PostgreSQL19, upperCaseKeywords: false })
  }
}

const baseExtensions = (lang) => [
  highlightSpecialChars(),
  drawSelection(),
  bracketMatching(),
  syntaxHighlighting(houseHighlight),
  houseTheme,
  language(lang),
]

/**
 * Create an editable editor. `onChange` fires on every document change, which
 * is what drives the page's live re-render.
 */
export function createInput({ parent, doc = '', lang = 'graphql', minHeight, onChange }) {
  const view = new EditorView({
    parent,
    // The tab panels are handed to <ga-tabs> before they are in the document,
    // so a view left to infer its own root would mount its stylesheet into a
    // detached fragment and render unstyled. Pin the root to the document.
    root: document,
    state: EditorState.create({
      doc,
      extensions: [
        ...baseExtensions(lang),
        history(),
        indentOnInput(),
        keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
        EditorView.updateListener.of((u) => {
          if (u.docChanged && onChange) onChange(u.state.doc.toString())
        }),
        minHeight ? EditorView.theme({ '.cm-content': { minHeight } }) : [],
      ],
    }),
  })
  return view
}

/** Create a read-only editor for a generated pane. */
export function createOutput({ parent, doc = '', lang = 'sql' }) {
  return new EditorView({
    parent,
    root: document,
    state: EditorState.create({
      doc,
      extensions: [
        ...baseExtensions(lang),
        EditorState.readOnly.of(true),
        EditorView.editable.of(false),
      ],
    }),
  })
}

/** Replace an editor's whole document. */
export function setDoc(view, text) {
  view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text ?? '' } })
}

/** The editor's current text. */
export function getDoc(view) {
  return view.state.doc.toString()
}
