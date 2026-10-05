import { lazy, Suspense } from 'react'
import type { ReactNode } from 'react'
import { HeadContent, Scripts, createRootRoute } from '@tanstack/react-router'

import appCss from '../styles.css?url'

// Devtools load only in development, so they stay out of the release bundle.
const Devtools = import.meta.env.DEV ? lazy(() => import('../integrations/devtools')) : () => null

// The shell is prerendered once and served for every path, so it holds no
// path-dependent markup; the nav lives in the _app layout.
export const Route = createRootRoute({
  head: () => ({
    meta: [
      { charSet: 'utf-8' },
      { name: 'viewport', content: 'width=device-width, initial-scale=1' },
      { title: 'Embolt' },
    ],
    links: [{ rel: 'stylesheet', href: appCss }],
  }),
  shellComponent: RootDocument,
})

function RootDocument({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <head>
        <HeadContent />
      </head>
      <body className="bg-zinc-50 text-zinc-900 antialiased dark:bg-zinc-950 dark:text-zinc-100">
        {children}
        <Suspense>
          <Devtools />
        </Suspense>
        <Scripts />
      </body>
    </html>
  )
}
