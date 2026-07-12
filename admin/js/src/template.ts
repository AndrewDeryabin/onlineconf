import * as CodeMirror from 'codemirror';
import 'codemirror/addon/mode/overlay';

import { ParamType } from './api';

// The variables the server expands; every other non-path name is substituted
// by the updater from its host-local configuration. Seeded with the known
// defaults and refreshed from /ui-config (see UIConfigProvider), the
// resolver's canonical list, so the classification cannot drift.
let serverVariables = new Set(['hostname', 'hostname -s', 'short_hostname', 'hostname -i', 'ip']);

export function setTemplateVars(vars: string[]) {
	serverVariables = new Set(vars);
}

export type TemplateVarClass = 'path' | 'server' | 'client';

// classifyTemplateVar tells where a ${...} placeholder is expanded: 'path'
// and 'server' server-side, 'client' by the updater (or to an empty string
// where its variables do not define the name).
export function classifyTemplateVar(name: string): TemplateVarClass {
	if (name.startsWith('/')) {
		return 'path';
	}
	return serverVariables.has(name) ? 'server' : 'client';
}

// The same non-greedy pattern the resolver substitutes by; used with split()
// so odd-indexed fragments are the placeholders.
export const templateTokenRe = /(\$\{.*?\})/g;

export function templateVarNames(value: string): string[] {
	const names: string[] = [];
	const tokens = value.split(templateTokenRe);
	for (let i = 1; i < tokens.length; i += 2) {
		names.push(tokens[i].slice(2, -1));
	}
	return names;
}

const templateModeSpec = () => ({
	startState: () => ({ variable: false }),
	token: (stream: CodeMirror.StringStream, state: { variable: boolean }) => {
		if (state.variable && stream.skipTo('}')) {
			stream.next();
			state.variable = false;
			const match = stream.current().match(/\$\{(.*)\}$/);
			const name = match ? match[1] : '';
			return classifyTemplateVar(name) === 'client' ? 'variable-3' : 'variable-2';
		} else if (!state.variable && stream.skipTo('${')) {
			state.variable = true;
			return null;
		} else {
			stream.skipToEnd();
			return null;
		}
	}
});

CodeMirror.defineMode('template', templateModeSpec);
CodeMirror.defineMIME('application/x-template', 'template');

// Flagged values keep their declared type, so their editors overlay the
// placeholder highlighting on top of the base syntax.
CodeMirror.defineMode('template+json', config =>
	CodeMirror.overlayMode(CodeMirror.getMode(config, 'application/json'), templateModeSpec()));
CodeMirror.defineMode('template+yaml', config =>
	CodeMirror.overlayMode(CodeMirror.getMode(config, 'yaml'), templateModeSpec()));

// editorMode picks the CodeMirror mode for a value: its mime doubles as the
// mode name, and the template flag swaps in the overlay variant.
export function editorMode(type: ParamType, template?: boolean): string {
	if (!template) {
		return type;
	}
	switch (type) {
		case 'application/json':
			return 'template+json';
		case 'application/x-yaml':
			return 'template+yaml';
		default:
			return 'template';
	}
}
