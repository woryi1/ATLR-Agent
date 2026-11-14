<script lang="ts">
  // ATLR-Agent brand mark — a five-petal sakura (cherry blossom), redrawn from
  // the former octopus head so the app carries the caoxiang rebuild's own
  // identity. Same footprint as favicon.svg, exposed as a Svelte component so
  // the header, sidebar and first-run panel stay in sync.
  interface Props {
    size?: number | string
    class?: string
  }
  let { size = 26, class: className = '' }: Props = $props()

  // One petal drawn pointing up from the flower centre (50,54), with the notch
  // at its tip that a sakura petal is known for. The flower is this petal
  // rotated five times around the centre.
  const PETAL = 'M50 54 C38 48 31 34 40 21 C45 14 50 17 50 25 C50 17 55 14 60 21 C69 34 62 48 50 54 Z'
  const ANGLES = [0, 72, 144, 216, 288]
</script>

<svg
  class={className}
  width={size}
  height={size}
  viewBox="0 0 100 100"
  fill="none"
  xmlns="http://www.w3.org/2000/svg"
  aria-label="ATLR-Agent"
>
  <defs>
    <linearGradient id="atlr-petal" x1="50" y1="14" x2="50" y2="54" gradientUnits="userSpaceOnUse">
      <stop offset="0" stop-color="#FFC2D1"/>
      <stop offset="1" stop-color="#FF8FAB"/>
    </linearGradient>
  </defs>
  <rect width="100" height="100" rx="22" fill="#FFF0F4"/>
  {#each ANGLES as a (a)}
    <path d={PETAL} transform={`rotate(${a} 50 54)`} fill="url(#atlr-petal)" stroke="#F27BA2" stroke-width="1.5" stroke-linejoin="round"/>
  {/each}
  <circle cx="50" cy="54" r="5.5" fill="#F6C445"/>
</svg>
