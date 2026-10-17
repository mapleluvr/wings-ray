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
import { Activity, BarChart3, CalendarClock, WalletCards } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { IconBadge, type IconBadgeTone } from '@/components/ui/icon-badge'
import { Skeleton } from '@/components/ui/skeleton'
import type { UserSubscription } from '@/features/subscriptions/types'
import { formatNumber, formatQuota } from '@/lib/format'

import type { UserWalletData } from '../types'

interface WalletStatsCardProps {
  user: UserWalletData | null
  /** Active subscriptions of the current user; drives the period usage stat. */
  subscriptions?: UserSubscription[]
  loading?: boolean
}

export function WalletStatsCard(props: WalletStatsCardProps) {
  const { t } = useTranslation()
  if (props.loading) {
    return (
      <div className='grid grid-cols-2 divide-x rounded-lg border sm:grid-cols-4'>
        {['balance', 'usage', 'requests', 'subscription'].map((key) => (
          <div key={key} className='min-w-0 px-2.5 py-2.5 sm:px-5 sm:py-4'>
            <Skeleton className='h-3.5 w-full' />
            <Skeleton className='mt-2 h-6 w-full sm:h-7' />
            <Skeleton className='mt-1.5 hidden h-3.5 w-24 md:block' />
          </div>
        ))}
      </div>
    )
  }

  const activeSubscriptions = props.subscriptions ?? []
  const hasUnlimited = activeSubscriptions.some(
    (subscription) => subscription.amount_total <= 0
  )
  // Period usage only: summing per-subscription remainders would invent an
  // aggregate balance the backend never grants (it charges one subscription
  // per request, and an unlimited subscription would contribute a negative).
  const periodUsed = activeSubscriptions.reduce(
    (sum, subscription) => sum + subscription.amount_used,
    0
  )
  const periodValue = hasUnlimited
    ? `${formatNumber(periodUsed)} · ${t('Unlimited')}`
    : formatNumber(periodUsed)
  const periodDescription = activeSubscriptions.length
    ? t('Current period; per-subscription remaining is shown in the plans card')
    : t('No active subscription')

  const stats: {
    label: string
    value: string
    description: string
    icon: typeof WalletCards
    tone: IconBadgeTone
  }[] = [
    {
      label: t('Wallet balance'),
      value: formatQuota(props.user?.quota ?? 0),
      description: t('Wallet quota only; subscription charges are separate'),
      icon: WalletCards,
      tone: 'success',
    },
    {
      label: t('Total charges'),
      value: formatQuota(props.user?.used_quota ?? 0),
      description: t('Includes subscription and wallet charges'),
      icon: BarChart3,
      tone: 'info',
    },
    {
      label: t('API Requests'),
      value: formatNumber(props.user?.request_count ?? 0),
      description: t('Total requests made'),
      icon: Activity,
      tone: 'chart-4',
    },
    {
      label: t('Subscription usage (this period)'),
      value: periodValue,
      description: periodDescription,
      icon: CalendarClock,
      tone: 'chart-5',
    },
  ]

  return (
    <div className='grid grid-cols-2 divide-x rounded-lg border sm:grid-cols-4'>
      {stats.map((item) => (
        <div key={item.label} className='min-w-0 px-2.5 py-2.5 sm:px-5 sm:py-4'>
          <div className='flex items-center gap-1.5 sm:gap-2.5'>
            <IconBadge tone={item.tone} size='stat'>
              <item.icon />
            </IconBadge>
            <div className='text-muted-foreground truncate text-[11px] font-medium tracking-wider uppercase sm:text-xs'>
              {item.label}
            </div>
          </div>

          <div className='text-foreground mt-1.5 font-mono text-sm font-bold tracking-tight break-all tabular-nums sm:mt-2.5 sm:text-2xl'>
            {item.value}
          </div>
          <div className='text-muted-foreground/60 mt-1 hidden text-xs md:block'>
            {item.description}
          </div>
        </div>
      ))}
    </div>
  )
}
