export default function UnsafeEmbedNotice({
  url,
  what,
}: {
  url: string
  what: 'preview' | 'terminal'
}) {
  return (
    <div className="embed-blocked" role="status">
      <p className="embed-blocked-h">Opening in a new tab instead.</p>
      <p className="embed-blocked-d">
        This server has no separate preview domain configured, so embedding a sandbox here
        would let sandbox code reach your dashboard. Everything works normally in a tab.
      </p>
      <a className="btn primary" href={url} target="_blank" rel="noopener noreferrer">
        Open the {what} in a new tab ↗
      </a>
      {(window.__SANDBOXD__?.mode ?? 'user') === 'admin' && (
        <p className="embed-blocked-admin">
          Admin: set <code>SANDBOXD_SANDBOX_DOMAIN</code> to re-enable embedded previews.
        </p>
      )}
    </div>
  )
}
