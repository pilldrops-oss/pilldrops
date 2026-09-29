import '@testing-library/jest-dom/vitest'

// Preview tokens are cached for the life of the module, so a value minted in one test
// would otherwise be served to the next.
import { beforeEach } from 'vitest'
import { _resetPreviewTokenCache } from '../src/lib/previewToken'
beforeEach(() => _resetPreviewTokenCache())
