import { describe, expect, it } from 'vitest'
import DOMPurify from 'dompurify'
import { sanitizeSvg } from '../sanitize'

describe('sanitization after dependency upgrade', () => {
  it('retains document content while removing script and executable links', () => {
    const html = DOMPurify.sanitize('<h2>Usage</h2><p><strong>Safe</strong></p><script>alert(1)</script><img src="/logo.png" onerror="alert(1)"><a href="javascript:alert(1)">Link</a>')
    const container = document.createElement('div')
    container.innerHTML = html
    expect(container.querySelector('h2')?.textContent).toBe('Usage')
    expect(container.querySelector('strong')?.textContent).toBe('Safe')
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('img')?.hasAttribute('onerror')).toBe(false)
    expect(container.querySelector('a')?.hasAttribute('href')).toBe(false)
  })

  it('preserves native SVG paths and removes embedded active content', () => {
    const safe = sanitizeSvg('<svg viewBox="0 0 24 24" onload="alert(1)"><path d="M0 0 L24 24"/><script>alert(1)</script><foreignObject><iframe src="javascript:alert(1)"></iframe></foreignObject></svg>')
    expect(safe).toContain('<path d="M0 0 L24 24"')
    expect(safe).toContain('viewBox="0 0 24 24"')
    expect(safe).not.toMatch(/onload|script|foreignObject|iframe/)
    expect(sanitizeSvg('')).toBe('')
  })
})
