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
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'

import {
  PRICING_KEYS,
  type PricingOptions,
} from '@/features/model-pricing/pricing'
import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { ModelRatioVisualEditor } from '../model-ratio-visual-editor'
import { RatioSettingsCard } from '../ratio-settings-card'

const previousUser = useAuthStore.getState().auth.user

let client: QueryClient | undefined

afterEach(() => {
  client?.clear()
  useAuthStore.getState().auth.setUser(previousUser)
  localStorage.clear()
  window.getSelection()?.removeAllRanges()
  vi.restoreAllMocks()
})

async function renderEditor() {
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/pricing') {
      return { data: { success: true, data: [], vendors: [] } }
    }
    return { data: { success: true, data: {} } }
  })
  const modelRatio = JSON.stringify({
    'gpt-4.1-mini': 0.2,
    'claude-sonnet': 1.5,
  })
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  client = queryClient
  const view = render(
    <QueryClientProvider client={client}>
      <ModelRatioVisualEditor
        savedModelPrice='{}'
        savedModelRatio={modelRatio}
        savedCacheRatio='{}'
        savedCreateCacheRatio='{}'
        savedCompletionRatio='{}'
        savedImageRatio='{}'
        savedAudioRatio='{}'
        savedAudioCompletionRatio='{}'
        savedBillingMode='{}'
        savedBillingExpr='{}'
        modelPrice='{}'
        modelRatio={modelRatio}
        cacheRatio='{}'
        createCacheRatio='{}'
        completionRatio='{}'
        imageRatio='{}'
        audioRatio='{}'
        audioCompletionRatio='{}'
        billingMode='{}'
        billingExpr='{}'
        onChange={vi.fn()}
        onSave={vi.fn()}
        isSaving={false}
      />
    </QueryClientProvider>
  )
  // The real page has pricing metadata loaded long before the user drags over
  // a model name, so settle the pricing query before interacting.
  await waitFor(() => expect(queryClient.isFetching()).toBe(0))
  return view
}

// A drag-select ends with mousedown/mouseup inside the row and the browser
// then fires `click` on the row. Only that final `click` is dispatched here so
// the text selection created by the drag is still active when the row opens
// the editor.
it('keeps the drag-selected model name highlighted when the row click opens the editor', async () => {
  await renderEditor()
  const row = await screen.findByRole('row', { name: /gpt-4\.1-mini/ })
  const nameCell = within(row).getByText('gpt-4.1-mini')
  window.getSelection()?.selectAllChildren(nameCell)
  expect(String(window.getSelection())).toBe('gpt-4.1-mini')

  fireEvent.click(nameCell)

  expect(
    await screen.findByRole('region', { name: 'Edit model pricing' })
  ).toBeInTheDocument()
  expect(nameCell).toBeInTheDocument()
  expect(String(window.getSelection())).toBe('gpt-4.1-mini')
})

it('keeps other rows mounted when a different model is opened for editing', async () => {
  await renderEditor()
  const targetRow = await screen.findByRole('row', { name: /gpt-4\.1-mini/ })
  const otherName = within(
    screen.getByRole('row', { name: /claude-sonnet/ })
  ).getByText('claude-sonnet')

  fireEvent.click(within(targetRow).getByText('gpt-4.1-mini'))

  expect(
    await screen.findByRole('region', { name: 'Edit model pricing' })
  ).toBeInTheDocument()
  expect(otherName).toBeInTheDocument()
})

it.each(['change subscription', 'clear wallet'])(
  'round-trips funding policies through the full system settings save path: %s',
  async (action) => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'root', role: ROLE.SUPER_ADMIN })
    const expression = 'tier("base", p * 2 + c * 6)'
    const options = Object.fromEntries(
      PRICING_KEYS.map((key) => [key, '{}'])
    ) as PricingOptions
    options['billing_setting.billing_mode'] = JSON.stringify({
      'funded-model': 'tiered_expr',
    })
    options['billing_setting.billing_expr'] = JSON.stringify({
      'funded-model': expression,
    })
    options['billing_setting.subscription_multiplier'] = JSON.stringify({
      'funded-model': 0.5,
      'funding-only': 0.8,
    })
    options['billing_setting.wallet_multiplier'] = JSON.stringify({
      'funded-model': 1.5,
    })
    const configured = {
      'billing_setting.billing_mode': 'tiered_expr',
      'billing_setting.billing_expr': expression,
      'billing_setting.subscription_multiplier': 0.5,
      'billing_setting.wallet_multiplier': 1.5,
    }
    vi.spyOn(api, 'get').mockImplementation(async (url) => {
      if (url === '/api/option/model_pricing') {
        return {
          data: {
            success: true,
            data: {
              options,
              empty_version: 'empty',
              entries: [
                {
                  model_name: 'funded-model',
                  version: 'original',
                  configured,
                  effective: configured,
                },
              ],
            },
          },
        }
      }
      return { data: { success: true, data: [], vendors: [] } }
    })
    const save = vi
      .spyOn(api, 'patch')
      .mockResolvedValue({ data: { success: true } })
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <RatioSettingsCard
          visibleTabs={['models']}
          toolPricesDefault='{}'
          modelDefaults={{
            ModelPrice: '{}',
            ModelRatio: '{}',
            CacheRatio: '{}',
            CreateCacheRatio: '{}',
            CompletionRatio: '{}',
            ImageRatio: '{}',
            AudioRatio: '{}',
            AudioCompletionRatio: '{}',
            ExposeRatioEnabled: false,
            BillingMode: '{}',
            BillingExpr: '{}',
            PluginBillingExpr: '{}',
          }}
          groupDefaults={{
            GroupRatio: '{}',
            TopupGroupRatio: '{}',
            UserUsableGroups: '{}',
            GroupGroupRatio: '{}',
            AutoGroups: '[]',
            MaxTokenAutoGroups: 1,
            DefaultUseAutoGroup: false,
            GroupSpecialUsableGroup: '{}',
          }}
        />
      </QueryClientProvider>
    )
    const row = await screen.findByRole('row', { name: /funded-model/ })
    fireEvent.click(within(row).getByText('funded-model'))
    const editor = within(
      await screen.findByRole('region', { name: 'Edit model pricing' })
    )
    const subscription = editor.getByRole('textbox', {
      name: 'Subscription multiplier',
    })
    const wallet = editor.getByRole('textbox', { name: 'Wallet multiplier' })
    expect(subscription).toHaveValue('0.5')
    expect(wallet).toHaveValue('1.5')
    expect(
      screen.getByRole('row', { name: /funding-only/ })
    ).toBeInTheDocument()
    fireEvent.change(action === 'change subscription' ? subscription : wallet, {
      target: { value: action === 'change subscription' ? '0.7' : '' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Save model prices' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(save.mock.calls[0][0]).toBe('/api/option/model_pricing')
    const request = save.mock.calls[0][1] as {
      changes: Array<{
        model_name: string
        expected_version: string
        pricing: Record<string, unknown>
      }>
    }
    expect(request.changes).toHaveLength(1)
    expect(request.changes[0]).toMatchObject({
      model_name: 'funded-model',
      expected_version: 'original',
    })
    expect(
      request.changes[0].pricing['billing_setting.subscription_multiplier']
    ).toBe(action === 'change subscription' ? 0.7 : 0.5)
    if (action === 'change subscription') {
      expect(
        request.changes[0].pricing['billing_setting.wallet_multiplier']
      ).toBe(1.5)
    } else {
      expect(request.changes[0].pricing).not.toHaveProperty(
        'billing_setting.wallet_multiplier'
      )
    }
    expect(request.changes[0].pricing['billing_setting.billing_expr']).toBe(
      expression
    )
  }
)
