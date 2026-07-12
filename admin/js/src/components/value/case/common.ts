import { ParamType } from '../../../api';

export const caseConditions = ['server', 'group', 'datacenter', 'service'] as const;
export type CaseConditions = typeof caseConditions[number];

export interface Case extends Partial<Record<CaseConditions, string>> {
	mime: ParamType;
	value: string | null;
	// present (true) only on flagged branches, so unflagged case values keep
	// their historical JSON shape byte for byte
	template?: boolean;
}
