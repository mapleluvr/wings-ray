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
import { render, screen, waitFor } from '@testing-library/react'
import { expect, it, vi } from 'vitest'

import { getSelfSubscriptionFull } from '@/features/subscriptions/api'
import { useStatus } from '@/hooks/use-status'
import { useSystemConfig } from '@/hooks/use-system-config'
import { getSelf } from '@/lib/api'
import { formatQuota } from '@/lib/format'

import { Wallet } from '../index'

vi.mock('@/components/layout', () => {
  const SectionPageLayout = ({ children }: { children?: React.ReactNode }) => (
    <div>{children}</div>
  )
  SectionPageLayout.Title = ({ children }: { children?: React.ReactNode }) => (
    <div>{children}</div>
  )
  SectionPageLayout.Content = ({
    children,
  }: {
    children?: React.ReactNode
  }) => <div>{children}</div>
  return { SectionPageLayout }
})
vi.mock('@/hooks/use-status', () => ({
  useStatus: vi.fn(() => ({ status: {} })),
}))
vi.mock('@/hooks/use-system-config', () => ({
  useSystemConfig: vi.fn(() => ({
    currency: { quotaDisplayType: 'USD', usdExchangeRate: 1 },
  })),
}))
vi.mock('@/lib/api', () => ({ getSelf: vi.fn() }))
vi.mock('@/features/subscriptions/api', () => ({
  getSelfSubscriptionFull: vi.fn(),
}))
vi.mock('../components/affiliate-rewards-card', () => ({
  AffiliateRewardsCard: () => null,
}))
vi.mock('../components/dialogs/billing-history-dialog', () => ({
  BillingHistoryDialog: () => null,
}))
vi.mock('../components/dialogs/creem-confirm-dialog', () => ({
  CreemConfirmDialog: () => null,
}))
vi.mock('../components/dialogs/payment-confirm-dialog', () => ({
  PaymentConfirmDialog: () => null,
}))
vi.mock('../components/dialogs/transfer-dialog', () => ({
  TransferDialog: () => null,
}))
vi.mock('../components/recharge-form-card', () => ({
  RechargeFormCard: () => null,
}))
vi.mock('../components/subscription-plans-card', () => ({
  SubscriptionPlansCard: () => null,
}))
vi.mock('../hooks', () => ({
  useTopupInfo: () => ({ topupInfo: null, presetAmounts: [], loading: false }),
  usePayment: () => ({
    amount: 0,
    calculating: false,
    processing: false,
    calculatePaymentAmount: vi.fn(),
    processPayment: vi.fn(),
  }),
  useAffiliate: () => ({
    affiliateLink: '',
    loading: false,
    transferQuota: vi.fn(),
    transferring: false,
  }),
  useRedemption: () => ({ redeeming: false, redeemCode: vi.fn() }),
  useCreemPayment: () => ({
    processing: false,
    processCreemPayment: vi.fn(),
  }),
  useWaffoPayment: () => ({ processing: false, processWaffoPayment: vi.fn() }),
  useWaffoPancakePayment: () => ({
    processing: false,
    processWaffoPancakePayment: vi.fn(),
  }),
}))

const walletUser = {
  id: 1,
  username: 'wallet-user',
  quota: 987654321,
  used_quota: 555000,
  request_count: 7,
}

function mocked() {
  return {
    getSelf: vi.mocked(getSelf),
    getSelfSubscriptionFull: vi.mocked(getSelfSubscriptionFull),
    useStatus: vi.mocked(useStatus),
    useSystemConfig: vi.mocked(useSystemConfig),
  }
}

it('renders wallet stats when getSelf succeeds even if the subscription fetch rejects', async () => {
  const mocks = mocked()
  mocks.useStatus.mockReturnValue({ status: {} } as ReturnType<
    typeof useStatus
  >)
  mocks.useSystemConfig.mockReturnValue({
    currency: { quotaDisplayType: 'USD', usdExchangeRate: 1 },
  } as ReturnType<typeof useSystemConfig>)
  mocks.getSelf.mockResolvedValue({
    success: true,
    data: walletUser,
    message: '',
  } as never)
  mocks.getSelfSubscriptionFull.mockRejectedValue(new Error('network down'))

  render(<Wallet />)

  await waitFor(() => {
    expect(screen.getByText('Wallet balance')).toBeVisible()
  })
  expect(screen.getByText(formatQuota(walletUser.quota))).toBeVisible()
  expect(screen.getByText('Total charges')).toBeVisible()
  expect(screen.getByText(formatQuota(walletUser.used_quota))).toBeVisible()
})

it('loads subscription usage through the same fetch when it succeeds', async () => {
  const mocks = mocked()
  mocks.useStatus.mockReturnValue({ status: {} } as ReturnType<
    typeof useStatus
  >)
  mocks.useSystemConfig.mockReturnValue({
    currency: { quotaDisplayType: 'USD', usdExchangeRate: 1 },
  } as ReturnType<typeof useSystemConfig>)
  mocks.getSelf.mockResolvedValue({
    success: true,
    data: walletUser,
    message: '',
  } as never)
  mocks.getSelfSubscriptionFull.mockResolvedValue({
    success: true,
    data: {
      subscriptions: [
        {
          subscription: {
            id: 1,
            user_id: 1,
            plan_id: 1,
            status: 'active',
            start_time: 0,
            end_time: 0,
            amount_total: 0,
            amount_used: 250,
          },
        },
      ],
    },
    message: '',
  } as never)

  render(<Wallet />)

  await waitFor(() => {
    expect(screen.getByText('250 · Unlimited')).toBeVisible()
  })
  expect(screen.getByText(formatQuota(walletUser.quota))).toBeVisible()
})
