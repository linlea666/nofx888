export const formatPrice = (value?: number | null, fallback = '--') => {
  if (value === undefined || value === null || Number.isNaN(value)) return fallback
  const maxDecimals = Math.abs(value) >= 100 ? 2 : 4
  return new Intl.NumberFormat(undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: maxDecimals,
  }).format(value)
}

export const formatQuantity = (value?: number | null, fallback = '--') => {
  if (value === undefined || value === null || Number.isNaN(value)) return fallback
  return new Intl.NumberFormat(undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 6,
  }).format(value)
}

export const formatCurrency = (value?: number | null, suffix = ' USDT', fallback = '--') => {
  if (value === undefined || value === null || Number.isNaN(value)) return fallback
  const formatted = new Intl.NumberFormat(undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 2,
  }).format(value)
  return `${formatted}${suffix}`
}
