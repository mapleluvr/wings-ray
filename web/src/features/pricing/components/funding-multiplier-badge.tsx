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
import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import { toIntlLocale } from '@/i18n/languages'
import { cn } from '@/lib/utils'

import type { PricingModel } from '../types'

interface FundingMultiplierBadgeProps {
  model: PricingModel
  appearance?: 'default' | 'caption'
  className?: string
}

/**
 * Model-level deduction policy marker for the public pricing surfaces.
 *
 * The catalog publishes a source only when the model has an explicit entry, so
 * a missing field means "no adjustment" and the badge stays hidden. Prices are
 * never recomputed here: list prices remain the displayed price and these
 * multipliers only change the user's actual charge on eligible requests.
 */
export function FundingMultiplierBadge(props: FundingMultiplierBadgeProps) {
  const { t, i18n } = useTranslation()
  const { subscription_multiplier: subscription, wallet_multiplier: wallet } =
    props.model
  const adjusted = [subscription, wallet].some(
    (value) => value !== undefined && value !== 1
  )
  if (!adjusted) {
    return null
  }

  // Funding multipliers can be much smaller than the ordinary 2-decimal
  // formatter supports; preserve their configured precision in this display.
  const multiplierFormat = new Intl.NumberFormat(
    toIntlLocale(i18n.resolvedLanguage || i18n.language),
    { maximumSignificantDigits: 15 }
  )
  const label = t('Subscription {{subscription}}× · Recharge {{wallet}}×', {
    subscription: multiplierFormat.format(subscription ?? 1),
    wallet: multiplierFormat.format(wallet ?? 1),
  })
  const policy = t(
    'Model-level deduction policy. Only eligible HTTP/SSE text-token expression requests are adjusted; channel usage always keeps list price.'
  )
  const isCaption = props.appearance === 'caption'

  return (
    <StatusBadge
      label={label}
      variant='info'
      type={isCaption ? 'text' : undefined}
      copyable={false}
      size='sm'
      className={cn(isCaption && 'text-xs font-normal', props.className)}
      title={policy}
      aria-label={`${label}. ${policy}`}
    />
  )
}
