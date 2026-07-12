import * as React from 'react';
import { useTranslation } from 'react-i18next';
import { Theme } from '@mui/material/styles';

import makeStyles from '@mui/styles/makeStyles';

import { classifyTemplateVar, templateTokenRe } from '../../template';
import { NonNullValueProps } from './common';
import TextValueView from './text/TextValueView';
import TextValueEdit from './text/TextValueEdit';

const usePreviewStyles = makeStyles((theme: Theme) => ({
	root: {
		color: theme.palette.text.primary,
	},
	variable: {
		textDecoration: 'none',
		color: theme.palette.onlineconf.templateVariable,
	},
	clientVariable: {
		color: theme.palette.warning.main,
	},
}));

function TemplateValuePreview(props: NonNullValueProps) {
	const classes = usePreviewStyles();
	const { t } = useTranslation();
	const tokens = props.value.split(templateTokenRe);
	return (
		<span className={classes.root}>
			{tokens.map((token: string, i: number) => {
				if (i % 2 === 0) {
					return <span key={i}>{token}</span>;
				}
				const name = token.slice(2, -1);
				switch (classifyTemplateVar(name)) {
					case 'path':
						return <a key={i} href={'#' + name} className={classes.variable} onClick={event => event.stopPropagation()}>{token}</a>;
					case 'server':
						return <span key={i} className={classes.variable}>{token}</span>;
					default:
						return <span key={i} className={classes.clientVariable} title={t('param.template.clientVar')}>{token}</span>;
				}
			})}
		</span>
	);
}

const templateValue = {
	preview: TemplateValuePreview,
	view: TextValueView,
	edit: TextValueEdit,
};
export default templateValue;
