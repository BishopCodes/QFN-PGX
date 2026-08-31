// Console chart glue — TanStack Charts via its vanilla DOM host.
// Bundled by `make chartjs` into web/vendor/tanstack-charts.iife.js
// (esbuild, no runtime deps left outside the file; offline-safe embed).
import { defineChart, lineY } from '@tanstack/charts'
import { scaleLinear } from '@tanstack/charts/scales/linear'
import { mountChart } from '@tanstack/charts/dom'

export function definition(rows) {
  return defineChart({
    marks: [
      lineY(rows, { x: 't', y: 'gen', stroke: '#3fd08a', strokeWidth: 1.6 }),
      lineY(rows, { x: 't', y: 'prompt', stroke: '#4cc2ff', strokeWidth: 1.2 }),
      lineY(rows, { x: 't', y: 'pct', yScale: 'pct', stroke: '#f0b429', strokeWidth: 1, strokeOpacity: 0.9 }),
    ],
    scales: {
      x: { scale: scaleLinear },
      y: { scale: scaleLinear, nice: true },
      pct: { scale: scaleLinear, domain: [0, 100] },
    },
  })
}

// create(el) → { update(rows), destroy() }
export function create(el) {
  let host = null
  return {
    update(rows) {
      const definition_ = definition(rows)
      if (host) host.update({ definition: definition_, height: 170 })
      else host = mountChart(el, { definition: definition_, height: 170, ariaLabel: 'tokens per second and percent usage, last five minutes' })
    },
    destroy() { host?.destroy(); host = null },
  }
}

if (typeof window !== 'undefined') window.QfnChart = { create }
