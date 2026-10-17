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
import { render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'

import type { UserSubscription } from '@/features/subscriptions/types'
import { formatNumber, formatQuota } from '@/lib/format'

import { WalletStatsCard } from '../components/wallet-stats-card'
import type { UserWalletData } from '../types'

const user = {
  id: 1,
  username: 'wallet-user',
  quota: 123456789,
  used_quota: 500000,
  request_count: 42,
} as UserWalletData

function subscription(overrides: Partial<UserSubscription> = {}) {
  return {
    id: 1,
    user_id: 1,
    plan_id: 1,
    status: 'active',
    start_time: 0,
    end_time: 0,
    amount_total: 1000,
    amount_used: 100,
    ...overrides,
  } as UserSubscription
}

it('shows the wallet-scoped stats instead of the shared labels', () => {
  render(<WalletStatsCard user={user} />)
  expect(screen.getByText('Wallet balance')).toBeVisible()
  expect(screen.getByText('Total charges')).toBeVisible()
  expect(screen.getByText('API Requests')).toBeVisible()
  expect(screen.getByText('Subscription usage (this period)')).toBeVisible()
  expect(screen.queryByText('Current Balance')).not.toBeInTheDocument()
  expect(screen.queryByText('Total Usage')).not.toBeInTheDocument()
})

it('formats balance and charges through the shared quota formatter', () => {
  render(<WalletStatsCard user={user} />)
  expect(screen.getByText(formatQuota(user.quota))).toBeVisible()
  expect(screen.getByText(formatQuota(user.used_quota))).toBeVisible()
  expect(screen.getByText(formatNumber(user.request_count))).toBeVisible()
})

it('sums period usage across active subscriptions without inventing a remainder', () => {
  render(
    <WalletStatsCard
      user={user}
      subscriptions={[
        subscription({ amount_used: 100 }),
        subscription({ amount_used: 250, id: 2 }),
      ]}
    />
  )
  expect(screen.getByText(formatNumber(350))).toBeVisible()
  expect(screen.queryByText(formatNumber(1650))).not.toBeInTheDocument()
})

it('marks unlimited subscriptions instead of computing a negative remainder', () => {
  render(
    <WalletStatsCard
      user={user}
      subscriptions={[subscription({ amount_total: 0, amount_used: 100 })]}
    />
  )
  expect(screen.getByText(`${formatNumber(100)} · Unlimited`)).toBeVisible()
})

it('reports zero usage without an active subscription', () => {
  render(<WalletStatsCard user={user} subscriptions={[]} />)
  expect(screen.getByText(formatNumber(0))).toBeVisible()
  expect(screen.getByText('No active subscription')).toBeVisible()
})

it('keeps the four-column responsive grid on both the stats and the skeleton', () => {
  const { container, rerender } = render(<WalletStatsCard user={user} />)
  const grid = container.firstElementChild
  expect(grid).toHaveClass('grid-cols-2', 'sm:grid-cols-4')
  expect(grid).not.toHaveClass('grid-cols-3')

  rerender(<WalletStatsCard user={null} loading />)
  expect(container.firstElementChild).toHaveClass(
    'grid-cols-2',
    'sm:grid-cols-4'
  )
})
