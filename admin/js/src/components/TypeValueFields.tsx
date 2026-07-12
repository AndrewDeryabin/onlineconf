import * as React from 'react';
import { withTranslation, WithTranslation } from 'react-i18next';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';

import { types, templateEligible } from './common';
import { Case } from './value/case/common';
import { ParamType } from '../api';
import { ValueEdit } from './value';
import { TemplateConfigContext } from './UIConfig';

export interface TypeValueState {
	type: ParamType;
	value: string | null;
	template?: boolean;
}

interface TypeValueFieldsProps {
	type: ParamType;
	value: string | null;
	template?: boolean;
	onChange: (state: TypeValueState) => void;
	onError: (error: unknown) => void;
}

class TypeValueFields extends React.Component<TypeValueFieldsProps & WithTranslation> {

	static contextType = TemplateConfigContext;
	context!: React.ContextType<typeof TemplateConfigContext>;

	handleTypeChange = (event: React.ChangeEvent<HTMLInputElement>) => {
		const type = event.target.value as ParamType;
		if (type !== this.props.type) {
			// the flag survives a type change while the new type can carry it,
			// and is cleared (not failed) otherwise
			const state: TypeValueState = {
				type,
				value: this.props.value,
				template: templateEligible(type) ? this.props.template : undefined,
			};
			if (type === 'application/x-null') {
				state.value = null;
			} else if (type === 'application/x-case') {
				// wrapping into a case moves the flag onto the generated branch
				const branch: Case = { mime: this.props.type, value: this.props.value };
				if (this.props.template && templateEligible(this.props.type)) {
					branch.template = true;
				}
				state.value = JSON.stringify([branch]);
			} else if (this.props.value === null) {
				state.value = '';
			} else if (this.props.type === 'application/x-case') {
				const cases: Case[] = JSON.parse(this.props.value || '[]');
				const defaultCase = cases.filter(c => (c.server === undefined && c.group === undefined && c.datacenter === undefined && c.service === undefined))[0];
				if (defaultCase !== undefined) {
					// unwrapping a case lifts the default branch's flag back
					// onto the parameter
					state.value = defaultCase.value !== null ? defaultCase.value : '';
					if (templateEligible(type) && defaultCase.template) {
						state.template = true;
					}
				} else {
					state.value = '';
				}
			}
			this.props.onChange(state);
		}
	};

	handleValueChange = (value: string | null) => {
		this.props.onChange({ type: this.props.type, value, template: this.props.template });
	};

	handleTemplateChange = (event: React.ChangeEvent<HTMLInputElement>) => {
		this.props.onChange({ type: this.props.type, value: this.props.value, template: event.target.checked });
	};

	render() {
		const showTemplateSwitch = this.context.enabled && templateEligible(this.props.type);
		return (
			<React.Fragment>
				<TextField
					id="mime"
					select
					label={this.props.t('param.type')}
					value={this.props.type}
					variant="outlined"
					margin="dense"
					fullWidth
					onChange={this.handleTypeChange}
				>
					{types.map((type: ParamType) => (
						<MenuItem key={type} value={type}>{this.props.t(`param.types.${type}`)}</MenuItem>
					))}
				</TextField>
				{showTemplateSwitch && (
					<FormControlLabel
						control={<Switch checked={this.props.template || false} onChange={this.handleTemplateChange} color="primary"/>}
						label={this.props.t('param.template.flag')}
					/>
				)}
				<ValueEdit
					type={this.props.type}
					value={this.props.value}
					template={this.props.template && templateEligible(this.props.type)}
					onChange={this.handleValueChange}
					onError={this.props.onError}
				/>
			</React.Fragment>
		);
	}

}

export default withTranslation()(TypeValueFields);
