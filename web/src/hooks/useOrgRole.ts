import type { OrgRole } from '../types'

// OSS has no team orgs, so the caller always owns everything. Cloud replaces this body.
export const useOrgRole = (): OrgRole | null => 'owner'
