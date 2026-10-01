import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import Shell from '../../src/components/Shell'

// The session is an HttpOnly cookie the page cannot clear; only the server's
// logout route can, so sign-out must reach it as a real POST.
describe('Shell sign out', () => {
  it('posts to the logout route', () => {
    render(
      <MemoryRouter>
        <Routes>
          <Route element={<Shell />}>
            <Route path="*" element={null} />
          </Route>
        </Routes>
      </MemoryRouter>,
    )
    const form = screen.getByRole('button', { name: 'Sign out' }).closest('form')
    expect(form?.getAttribute('method')).toBe('post')
    expect(form?.getAttribute('action')).toBe('/dashboard/logout')
  })
})
