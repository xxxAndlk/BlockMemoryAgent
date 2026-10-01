// highlight.js core + 语言注册（TODO #26 阶段 H 语言集）。
// 从 FilePreview.vue 抽出为共享模块：FilePreview 面板与 FileViewer 全屏层共用同一
// 实例与语言集（core 单例，重复 registerLanguage 会被后注册覆盖同名语言，必须只注册一次）。
import hljs from 'highlight.js/lib/core'
import javascript from 'highlight.js/lib/languages/javascript'
import typescript from 'highlight.js/lib/languages/typescript'
import jsonLang from 'highlight.js/lib/languages/json'
import python from 'highlight.js/lib/languages/python'
import go from 'highlight.js/lib/languages/go'
import java from 'highlight.js/lib/languages/java'
import cpp from 'highlight.js/lib/languages/cpp'
import xml from 'highlight.js/lib/languages/xml'
import css from 'highlight.js/lib/languages/css'
import scss from 'highlight.js/lib/languages/scss'
import bash from 'highlight.js/lib/languages/bash'
import yaml from 'highlight.js/lib/languages/yaml'
import markdown from 'highlight.js/lib/languages/markdown'
import sql from 'highlight.js/lib/languages/sql'
import ini from 'highlight.js/lib/languages/ini'
import diff from 'highlight.js/lib/languages/diff'
import dockerfile from 'highlight.js/lib/languages/dockerfile'
import lua from 'highlight.js/lib/languages/lua'

hljs.registerLanguage('javascript', javascript)
hljs.registerLanguage('typescript', typescript)
hljs.registerLanguage('json', jsonLang)
hljs.registerLanguage('python', python)
hljs.registerLanguage('go', go)
hljs.registerLanguage('java', java)
hljs.registerLanguage('cpp', cpp)
hljs.registerLanguage('xml', xml)
hljs.registerLanguage('css', css)
hljs.registerLanguage('scss', scss)
hljs.registerLanguage('bash', bash)
hljs.registerLanguage('yaml', yaml)
hljs.registerLanguage('markdown', markdown)
hljs.registerLanguage('sql', sql)
hljs.registerLanguage('ini', ini)
hljs.registerLanguage('diff', diff)
hljs.registerLanguage('dockerfile', dockerfile)
hljs.registerLanguage('lua', lua)

/** 已注册语言白名单（hljs 未注册的语言不传入，走纯文本展示）。 */
export const HLJS_SUPPORTED = new Set(['javascript', 'typescript', 'json', 'python', 'go', 'java', 'cpp',
  'xml', 'css', 'scss', 'bash', 'yaml', 'sql', 'ini', 'diff', 'dockerfile', 'lua', 'markdown'])

export default hljs
