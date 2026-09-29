import type { CSSProperties } from 'react'

export function Skeleton({
  w = '100%',
  h = 14,
  r = 6,
  style,
}: { w?: number | string; h?: number | string; r?: number; style?: CSSProperties }) {
  return <span className="skeleton" style={{ width: w, height: h, borderRadius: r, ...style }} />
}

export function MetricsSkeleton({ count = 4 }: { count?: number }) {
  return (
    <div className="metrics">
      {Array.from({ length: count }).map((_, i) => (
        <div className="metric" key={i}>
          <Skeleton w={84} h={12} />
          <Skeleton w={64} h={28} style={{ marginTop: 14, display: 'block' }} />
        </div>
      ))}
    </div>
  )
}

export function RowsSkeleton({ rows = 4 }: { rows?: number }) {
  return (
    <div className="rows-skeleton">
      {Array.from({ length: rows }).map((_, i) => (
        <div className="rows-skeleton-row" key={i}>
          <Skeleton w={31} h={31} r={9} />
          <Skeleton w={`${40 + (i % 3) * 12}%`} h={13} />
          <Skeleton w={64} h={20} r={8} style={{ marginLeft: 'auto' }} />
        </div>
      ))}
    </div>
  )
}
