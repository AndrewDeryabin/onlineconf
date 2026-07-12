import { TreeNodeState } from './TreeNode';
import { ParamType, IParam } from '../api';
import { DistributiveOmit } from '@mui/types';

export interface ValueProps {
	type: ParamType;
	value: string | null;
	template?: boolean;
}

// Types whose values may carry the template flag: everything except the
// special types whose value is not free text. Must match templateEligible in
// the backend (admin/go/admin/parameter.go).
export function templateEligible(type: ParamType): boolean {
	switch (type) {
		case 'application/x-null':
		case 'application/x-symlink':
		case 'application/x-case':
		case 'application/x-template':
			return false;
		default:
			return true;
	}
}

export interface EditValueProps extends ValueProps {
	onChange: (value: string | null) => void;
	onError: (error: unknown) => void;
}

export interface IParamNode extends DistributiveOmit<IParam, 'children'> {
	children?: { [name: string]: IParamNode };
	state: TreeNodeState;
	selected?: boolean;
	match?: boolean;
	hidden?: boolean;
	forceOpen?: boolean;
	logLoading?: boolean;
	accessLoading?: boolean;
}

const typesObj: { [P in ParamType]: null } = {
	'application/x-null': null,
	'text/plain': null,
	'application/x-template': null,
	'application/json': null,
	'application/x-yaml': null,
	'application/x-symlink': null,
	'application/x-case': null,
	'application/x-list': null,
	'application/x-server': null,
	'application/x-server2': null,
};
export const types = Object.keys(typesObj) as ParamType[];

export interface ParamDialogProps {
	path: string;
	fullScreen?: boolean;
	onClose: () => void;
	onLoaded: () => void;
	onError: (error: unknown) => void;
}

export function smartCompare(a: string, b: string) {
	const amatch = a.match(/^(\D*?)(\d+)(\D*?)$/);
	const bmatch = b.match(/^(\D*?)(\d+)(\D*?)$/);
	if (amatch && bmatch && amatch[1] === bmatch[1]) {
		const anum = parseInt(amatch[2], 10);
		const bnum = parseInt(bmatch[2], 10);
		return (anum === bnum ? amatch[3] > bmatch[3] : anum > bnum) ? 1 : -1;
	}
	else {
		return a > b ? 1 : -1;
	}
}
