// Breadcrumb labels. Adding a route to App.tsx means adding it here; the Record types
// make a missing label a compile error instead of a wrong breadcrumb.

export const CUSTOMER_ROUTES = [
  '', 'projects', 'sandboxes', 'deploy', 'templates', 'vault', 'domains', 'usage', 'keys', 'docs', 'sandbox', 'functions', 'fn',
] as const

export const ADMIN_ROUTES = [
  '', 'overview', 'sessions', 'errors', 'feedback', 'waitlist', 'users',
  'fleet', 'cluster-sandboxes', 'images', 'add-node', 'system', 'sandboxes',
] as const

export type CustomerRoute = (typeof CUSTOMER_ROUTES)[number]
export type AdminRoute = (typeof ADMIN_ROUTES)[number]

const CUSTOMER_LABELS: Record<CustomerRoute, string> = {
  '': 'Sandboxes',
  projects: 'Projects',
  sandboxes: 'Sandboxes',
  deploy: 'Catalog',
  templates: 'Templates',
  vault: 'Vault',
  domains: 'Domains',
  usage: 'Usage',
  keys: 'API Keys',
  docs: 'Docs',
  // A detail page is labelled as the list it came from.
  sandbox: 'Sandboxes',
  functions: 'Functions',
  fn: 'Functions',
}

const ADMIN_LABELS: Record<AdminRoute, string> = {
  '': 'Overview',
  overview: 'Overview',
  sessions: 'Sessions',
  errors: 'Errors',
  feedback: 'Feedback',
  waitlist: 'Waitlist',
  users: 'Users',
  fleet: 'Fleet',
  'cluster-sandboxes': 'Cluster Sandboxes',
  images: 'Images',
  'add-node': 'Add Node',
  system: 'System / Scale',
  sandboxes: 'Cluster Sandboxes',
}

/** labelFor returns the breadcrumb label for a first path segment, falling back to the index label. */
export function labelFor(segment: string, isAdmin: boolean): string {
  if (isAdmin) return ADMIN_LABELS[segment as AdminRoute] ?? ADMIN_LABELS['']
  return CUSTOMER_LABELS[segment as CustomerRoute] ?? CUSTOMER_LABELS['']
}
