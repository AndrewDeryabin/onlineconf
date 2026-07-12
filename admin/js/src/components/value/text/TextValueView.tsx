import * as React from 'react';

import { editorMode } from '../../../template';
import { NonNullValueProps } from '../common';
import CodeMirror from '../../CodeMirror';

const TextValueView = (props: NonNullValueProps) => (
	<CodeMirror
		value={props.value}
		options={{
			mode: editorMode(props.type, props.template || props.type === 'application/x-template'),
			readOnly: true,
			tabindex: -1,

		}}
		onBeforeChange={() => null}
	/>
);

export default TextValueView;
