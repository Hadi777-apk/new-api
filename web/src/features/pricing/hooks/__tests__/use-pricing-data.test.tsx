/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { usePricingData } from '../use-pricing-data'

const quote = vi.hoisted(() => ({
  data: { model: 'gpt-image-2.5', price_cents: 5 as number | null },
  error: null as Error | null,
}))
vi.mock('@/features/agents/hooks', () => ({ useCustomerPrice: () => quote }))
vi.mock('@/hooks/use-status', () => ({
  useStatus: () => ({ status: { price: 1, usd_exchange_rate: 1 } }),
}))
const clients: QueryClient[] = []
afterEach(() => {
  clients.splice(0).forEach((client) => client.clear())
  quote.data.price_cents = 5
  quote.error = null
  vi.restoreAllMocks()
})
function mountPrices() {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      data: [
        'gpt-image-2.5',
        'gpt-image-2.5-2k',
        'gpt-image-2.5-4k',
        'gpt-image-2',
      ].map((model_name) => ({
        model_name,
        model_price:
          model_name.includes('-2k') || model_name.includes('-4k')
            ? 0.07
            : 0.06,
        quota_type: 1,
        enable_groups: ['default'],
      })),
      vendors: [],
      group_ratio: { default: 1 },
    },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  return renderHook(usePricingData, {
    wrapper: ({ children }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  })
}

test('the server quote applies five cents only to the migrated 2.5 model', async () => {
  const { result } = mountPrices()
  await waitFor(() => expect(result.current.models).toHaveLength(4))
  const base = result.current.models.find(
    (model) => model.model_name === 'gpt-image-2.5'
  )
  expect(base?.agent_price_cents).toBe(5)
  expect(base?.billing_mode).toBe('agent_image')
  for (const model of result.current.models.filter(
    (row) => row.model_name !== 'gpt-image-2.5'
  )) {
    expect(model.agent_price_cents).toBeUndefined()
    expect(model.model_price).toBe(
      model.model_name === 'gpt-image-2' ? 0.06 : 0.07
    )
  }
})

test('ordinary customers keep the six-cent base price', async () => {
  quote.data.price_cents = null
  const { result } = mountPrices()
  await waitFor(() => expect(result.current.models).toHaveLength(4))
  const base = result.current.models.find(
    (model) => model.model_name === 'gpt-image-2.5'
  )
  expect(base?.agent_price_cents).toBeUndefined()
  expect(base?.model_price).toBe(0.06)
})

test('a failed quote lookup marks only the applicable model unavailable', async () => {
  quote.error = new Error('quote unavailable')
  const { result } = mountPrices()
  await waitFor(() => expect(result.current.models).toHaveLength(4))
  expect(
    result.current.models.find((model) => model.model_name === 'gpt-image-2.5')
      ?.agent_price_unavailable
  ).toBe(true)
  expect(
    result.current.models.find(
      (model) => model.model_name === 'gpt-image-2.5-4k'
    )?.agent_price_unavailable
  ).toBeUndefined()
})
