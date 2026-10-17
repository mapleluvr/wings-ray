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
import { act, render, screen } from '@testing-library/react'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { FundingMultiplierBadge } from '../components/funding-multiplier-badge'
import { ModelDetailsContent } from '../components/model-details'
import type { PricingModel } from '../types'

vi.mock('@lobehub/icons', () => ({}))
vi.mock('@visactor/react-vchart', () => ({ VChart: () => null }))
vi.mock('@visactor/vchart', () => ({}))

function pricingModel(overrides: Partial<PricingModel> = {}): PricingModel {
  return {
    id: 1,
    model_name: 'example-model',
    quota_type: 0,
    model_ratio: 1,
    completion_ratio: 3,
    enable_groups: ['default'],
    ...overrides,
  }
}

const POLICY_KEY =
  'Model-level deduction policy. Only eligible HTTP/SSE text-token expression requests are adjusted; channel usage always keeps list price.'

const clients: QueryClient[] = []
beforeEach(() => {
  vi.spyOn(api, 'get').mockResolvedValue({ data: { data: { groups: [] } } })
})
afterEach(() => {
  for (const client of clients.splice(0)) client.clear()
  vi.restoreAllMocks()
})

function renderDetails(model: PricingModel) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  clients.push(client)
  return render(
    <QueryClientProvider client={client}>
      <ModelDetailsContent
        model={model}
        groupRatio={{ default: 1 }}
        usableGroup={{ default: { desc: '', ratio: 1 } }}
        endpointMap={{}}
        autoGroups={[]}
        priceRate={1}
        usdExchangeRate={1}
        tokenUnit='M'
      />
    </QueryClientProvider>
  )
}

it('renders both sources when at least one is adjusted', () => {
  render(
    <FundingMultiplierBadge
      model={pricingModel({
        subscription_multiplier: 1.5,
        wallet_multiplier: 0.9,
      })}
    />
  )
  expect(screen.getByText('Subscription 1.5× · Recharge 0.9×')).toBeVisible()
})

it('shows 1× for the unconfigured source instead of hiding it', () => {
  render(
    <FundingMultiplierBadge
      model={pricingModel({ subscription_multiplier: 0.5 })}
    />
  )
  expect(screen.getByText('Subscription 0.5× · Recharge 1×')).toBeVisible()
})

it.each([
  ['both fields absent', pricingModel()],
  [
    'explicit 1× entries',
    pricingModel({ subscription_multiplier: 1, wallet_multiplier: 1 }),
  ],
])('stays hidden for an unadjusted model with %s', (_name, model) => {
  const { container } = render(<FundingMultiplierBadge model={model} />)
  expect(container.childElementCount).toBe(0)
})

it('exposes the model-level policy through the accessible name', () => {
  render(
    <FundingMultiplierBadge
      model={pricingModel({
        subscription_multiplier: 1.5,
        wallet_multiplier: 0.9,
      })}
    />
  )
  const badge = screen.getByLabelText(/Model-level deduction policy/)
  expect(badge).toHaveAttribute(
    'aria-label',
    expect.stringContaining('Subscription 1.5× · Recharge 0.9×')
  )
})

it('preserves sub-2-decimal precision instead of rounding to zero', () => {
  render(
    <FundingMultiplierBadge
      model={pricingModel({ subscription_multiplier: 0.0000001 })}
    />
  )
  expect(
    screen.getByText('Subscription 0.0000001× · Recharge 1×')
  ).toBeVisible()
})

it('keeps the details block always rendered and treats absent fields as 1×', () => {
  renderDetails(pricingModel())
  expect(screen.getByText('Subscription deduction multiplier')).toBeVisible()
  expect(screen.getByText('Recharge deduction multiplier')).toBeVisible()
  expect(screen.getByText(POLICY_KEY)).toBeVisible()
  expect(screen.getAllByText('1×')).toHaveLength(2)
})

it('keeps a configured source next to the 1× default in details', () => {
  renderDetails(pricingModel({ wallet_multiplier: 0.5 }))
  expect(screen.getByText('0.5×')).toBeVisible()
  expect(screen.getByText('1×')).toBeVisible()
})

it('preserves sub-2-decimal precision in details', () => {
  renderDetails(pricingModel({ subscription_multiplier: 0.0000001 }))
  expect(screen.getByText('0.0000001×')).toBeVisible()
})

it('localizes the deduction policy badge across interface languages', async () => {
  const i18n = createInstance()
  await i18n.init({
    lng: 'en',
    resources: { en, fr, ja, ru, vi: viLocale, zhCN: zh, zhTW },
    interpolation: { escapeValue: false },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <FundingMultiplierBadge
        model={pricingModel({
          subscription_multiplier: 0.0000001,
          wallet_multiplier: 0.9,
        })}
      />
    </I18nextProvider>
  )
  for (const [language, expected] of [
    ['zhCN', '订阅 0.0000001× · 充值 0.9×'],
    ['zhTW', '訂閱 0.0000001× · 儲值 0.9×'],
    ['en', 'Subscription 0.0000001× · Recharge 0.9×'],
    ['fr', 'Abonnement 0,0000001× · Rechargement 0,9×'],
    ['ru', 'Подписка 0,0000001× · Пополнение 0,9×'],
    ['ja', 'サブスク 0.0000001× · チャージ 0.9×'],
    ['vi', 'Gói đăng ký 0,0000001× · Nạp 0,9×'],
    // A missing translation falls back to the interpolated source key and the
    // formatter degrades to the runtime default locale instead of crashing.
    ['invalid-interface-language', /Subscription 0[.,]0000001×/],
  ] as const) {
    await act(async () => {
      await i18n.changeLanguage(language)
    })
    expect(await screen.findByText(expected as string)).toBeVisible()
  }
})

it('localizes the details deduction labels across interface languages', async () => {
  const i18n = createInstance()
  await i18n.init({
    lng: 'en',
    resources: { en, fr, ja, ru, vi: viLocale, zhCN: zh, zhTW },
    interpolation: { escapeValue: false },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider
        client={
          new QueryClient({ defaultOptions: { queries: { retry: false } } })
        }
      >
        <ModelDetailsContent
          model={pricingModel({ subscription_multiplier: 0.5 })}
          groupRatio={{ default: 1 }}
          usableGroup={{ default: { desc: '', ratio: 1 } }}
          endpointMap={{}}
          autoGroups={[]}
          priceRate={1}
          usdExchangeRate={1}
          tokenUnit='M'
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  for (const [language, expected] of [
    ['zhCN', '订阅扣费倍率'],
    ['zhTW', '訂閱扣費倍率'],
    ['en', 'Subscription deduction multiplier'],
    ['fr', 'Multiplicateur de déduction (abonnement)'],
    ['ru', 'Множитель списания по подписке'],
    ['ja', 'サブスク課金倍率'],
    ['vi', 'Hệ số trừ tiền gói đăng ký'],
    ['invalid-interface-language', 'Subscription deduction multiplier'],
  ] as const) {
    await act(async () => {
      await i18n.changeLanguage(language)
    })
    expect(await screen.findByText(expected)).toBeVisible()
  }
  expect(screen.getByText('0.5×')).toBeVisible()
})
