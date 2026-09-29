export default function Home() {
  return (
    <main style={{
      minHeight: '100vh', display: 'flex', flexDirection: 'column',
      alignItems: 'center', justifyContent: 'center', gap: 12,
      fontFamily: 'ui-monospace, SF Mono, Menlo, monospace',
      background: '#131210', color: '#f4f1e8',
    }}>
      <h1 style={{ margin: 0, fontSize: 34, letterSpacing: '-0.03em' }}>
        ▲ It&rsquo;s alive.
      </h1>
      <p style={{ margin: 0, color: '#a8a294', fontSize: 15 }}>
        Next.js, served from your sandbox&rsquo;s preview URL.
      </p>
      <code style={{
        marginTop: 10, padding: '10px 16px', borderRadius: 6,
        background: '#1a1815', border: '1px solid rgba(244,240,230,.13)',
        color: '#35c98a', fontSize: 13,
      }}>
        edit /root/app/pages/index.jsx — hot reload is on
      </code>
    </main>
  );
}
