import * as React from 'react';
import { useTranslation } from 'react-i18next';
import { Theme } from '@mui/material/styles';
import makeStyles from '@mui/styles/makeStyles';
import { InputBaseComponentProps } from '@mui/material/InputBase';
import TextField from '@mui/material/TextField';

import { classifyTemplateVar, editorMode, templateVarNames } from '../../../template';
import { getParam } from '../../../api';
import { EditNonnullValueProps } from '../common';
import CodeMirrorComponent from '../../CodeMirror';

const useStyles = makeStyles((theme: Theme) => ({
	field: {
		minHeight: 0,
	},
	inputRoot: {
		alignItems: 'stretch',
		minHeight: 0,
	},
	input: {
		height: 'auto',
	},
	codemirror: {
		width: 'inherit',
		height: '100%',
		'& > .CodeMirror': {
			height: '100%',
		},
	},
	helper: {
		padding: 0,
		margin: 0,
		listStyleType: 'none',
	},
	warning: {
		color: theme.palette.warning.main,
	},
	error: {
		color: theme.palette.error.main,
	},
}));

function CodeMirrorEditor(props: InputBaseComponentProps) {
	const { className, inputRef, onFocus, onBlur, onBeforeChange, mode, value } = props;
	const classes = useStyles();

	const editorRef = React.useRef<CodeMirror.Editor>();
	React.useImperativeHandle(inputRef, () => ({
		focus: () => editorRef.current?.focus(),
		value,
	}));

	return (
		<div className={className} onClick={() => editorRef.current?.focus()}>
			<CodeMirrorComponent
				value={value as string}
				options={{ mode, scrollbarStyle: 'null' }}
				className={classes.codemirror}
				editorDidMount={(editor) => { editorRef.current = editor }}
				onBeforeChange={onBeforeChange ? (editor, data, value) => onBeforeChange(value) : () => undefined}
				onFocus={onFocus ? (editor, event) => onFocus(event) : undefined}
				onBlur={onBlur ? (editor, event) => onBlur(event) : undefined}
			/>
		</div>
	);
}

// useMissingPaths checks the ${/path} placeholders of the value against the
// tree (debounced): a missing target expands to an empty string, which is
// almost always a typo or a not-yet-created parameter. Advisory only —
// admin-side resolution follows symlinks but cannot evaluate per-server case
// branches.
function useMissingPaths(value: string): string[] {
	const [missing, setMissing] = React.useState<string[]>([]);

	React.useEffect(() => {
		const paths = Array.from(new Set(templateVarNames(value).filter(name => classifyTemplateVar(name) === 'path')));
		if (paths.length === 0) {
			setMissing([]);
			return;
		}
		let cancelled = false;
		const timer = setTimeout(() => {
			Promise.all(paths.map(path =>
				getParam(path, 'resolve')
					.then(() => null)
					.catch(error => error.response?.status === 404 ? path : null)
			)).then(results => {
				if (!cancelled) {
					setMissing(results.filter((path): path is string => path !== null));
				}
			});
		}, 500);
		return () => {
			cancelled = true;
			clearTimeout(timer);
		};
	}, [value]);

	return missing;
}

interface TemplateHelperTextProps {
	value: string;
	structured: boolean;
}

function TemplateHelperText(props: TemplateHelperTextProps) {
	const classes = useStyles();
	const { t } = useTranslation();

	const clientVars = Array.from(new Set(templateVarNames(props.value).filter(name => classifyTemplateVar(name) === 'client')));
	const missingPaths = useMissingPaths(props.value);

	/* eslint-disable no-template-curly-in-string */
	return (
		<ul className={classes.helper}>
			<li>{'${hostname}'} - {t('param.template.hostname')}</li>
			<li>{'${short_hostname}'} - {t('param.template.shortHostname')}</li>
			<li>{'${ip}'} - {t('param.template.ip')}</li>
			<li>{'${/path/of/parameter}'} - {t('param.template.param')}</li>
			{clientVars.map(name => (
				<li key={name} className={classes.warning}>{'${' + name + '}'} - {t('param.template.clientVar')}</li>
			))}
			{props.structured && clientVars.length > 0 && (
				<li className={classes.error}>{t('param.template.typedClientVars')}</li>
			)}
			{missingPaths.map(path => (
				<li key={path} className={classes.warning}>{'${' + path + '}'} - {t('param.template.missingPath')}</li>
			))}
		</ul>
	);
	/* eslint-enable no-template-curly-in-string */
}

export default function TextValueEdit(props: EditNonnullValueProps) {
	const { onChange, type, template, value } = props;
	const { t } = useTranslation();
	const classes = useStyles();

	const isTemplate = template || type === 'application/x-template';
	const structured = type === 'application/json' || type === 'application/x-yaml';

	return (
		<TextField
			id="value"
			label={t('param.value')}
			fullWidth
			variant="outlined"
			margin="dense"
			helperText={isTemplate ? <TemplateHelperText value={value} structured={structured}/> : undefined}
			FormHelperTextProps={{ component: 'div' }}
			value={value}
			className={classes.field}
			InputProps={{
				inputComponent: CodeMirrorEditor,
				inputProps: { mode: editorMode(type, isTemplate), onBeforeChange: onChange, className: classes.input },
				className: classes.inputRoot,
			}}
		/>
	);
}
