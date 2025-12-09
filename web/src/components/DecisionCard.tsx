import { useState } from 'react'
import type { DecisionRecord } from '../types'
import { t, type Language } from '../i18n/translations'

interface DecisionCardProps {
  decision: DecisionRecord
  language: Language
}

export function DecisionCard({ decision, language }: DecisionCardProps) {
  const [showInputPrompt, setShowInputPrompt] = useState(false)
  const [showCoT, setShowCoT] = useState(false)

  const leaderNotional = decision.leader_notional ?? 0
  const leaderEquity = decision.leader_equity ?? 0
  const followerEquity = decision.follower_equity ?? 0
  const copyRatio = decision.copy_ratio ?? 0
  const followerNotional = decision.follower_notional ?? 0
  const followerQty = decision.follower_qty ?? 0
  const leaderPrice = decision.leader_price ?? 0
  const derivedPrice = followerQty > 0 ? followerNotional / followerQty : undefined
  const rawRatio = leaderEquity > 0 ? leaderNotional / leaderEquity : undefined
  const targetNotional = rawRatio && followerEquity > 0 ? rawRatio * followerEquity * (copyRatio / 100) : undefined

  const formatNumber = (value?: number, digits = 4) => {
    if (value === undefined || Number.isNaN(value)) return '--'
    return Number(value).toFixed(digits)
  }

  const renderBadge = (label: string, tone: 'ok' | 'warn' | 'info') => {
    const styles = {
      ok: { background: 'rgba(14, 203, 129, 0.1)', color: '#0ECB81' },
      warn: { background: 'rgba(246, 70, 93, 0.1)', color: '#F6465D' },
      info: { background: 'rgba(96, 165, 250, 0.12)', color: '#60a5fa' },
    }[tone]
    return (
      <span className="px-2 py-0.5 rounded text-[11px] font-semibold" style={styles}>
        {label}
      </span>
    )
  }

  return (
    <div
      className="rounded p-5 transition-all duration-300 hover:translate-y-[-2px]"
      style={{
        border: '1px solid #2B3139',
        background: '#1E2329',
        boxShadow: '0 2px 8px rgba(0, 0, 0, 0.3)',
      }}
    >
      <div className="flex items-start justify-between mb-3">
        <div>
          <div className="font-semibold" style={{ color: '#EAECEF' }}>
            {t('cycle', language)} #{decision.cycle_number}
          </div>
          <div className="text-xs" style={{ color: '#848E9C' }}>
            {new Date(decision.timestamp).toLocaleString()}
          </div>
        </div>
        <div
          className="px-3 py-1 rounded text-xs font-bold"
          style={
            decision.success
              ? { background: 'rgba(14, 203, 129, 0.1)', color: '#0ECB81' }
              : { background: 'rgba(246, 70, 93, 0.1)', color: '#F6465D' }
          }
        >
          {t(decision.success ? 'success' : 'failed', language)}
        </div>
      </div>

      {decision.input_prompt && (
        <div className="mb-3">
          <button
            onClick={() => setShowInputPrompt(!showInputPrompt)}
            className="flex items-center gap-2 text-sm transition-colors"
            style={{ color: '#60a5fa' }}
          >
            <span className="font-semibold">
              📥 {t('inputPrompt', language)}
            </span>
            <span className="text-xs">
              {showInputPrompt ? t('collapse', language) : t('expand', language)}
            </span>
          </button>
          {showInputPrompt && (
            <div
              className="mt-2 rounded p-4 text-sm font-mono whitespace-pre-wrap max-h-96 overflow-y-auto"
              style={{
                background: '#0B0E11',
                border: '1px solid #2B3139',
                color: '#EAECEF',
              }}
            >
              {decision.input_prompt}
            </div>
          )}
        </div>
      )}

      {decision.cot_trace && (
        <div className="mb-3">
          <button
            onClick={() => setShowCoT(!showCoT)}
            className="flex items-center gap-2 text-sm transition-colors"
            style={{ color: '#F0B90B' }}
          >
            <span className="font-semibold">
              📤 {t('aiThinking', language)}
            </span>
            <span className="text-xs">
              {showCoT ? t('collapse', language) : t('expand', language)}
            </span>
          </button>
          {showCoT && (
            <div
              className="mt-2 rounded p-4 text-sm font-mono whitespace-pre-wrap max-h-96 overflow-y-auto"
              style={{
                background: '#0B0E11',
                border: '1px solid #2B3139',
                color: '#EAECEF',
              }}
            >
              {decision.cot_trace}
            </div>
          )}
        </div>
      )}

      {decision.decisions && decision.decisions.length > 0 && (
        <div className="space-y-2 mb-3">
          {decision.decisions.map((action, index) => (
            <div
              key={`${action.symbol}-${index}`}
              className="flex items-center gap-2 text-sm rounded px-3 py-2"
              style={{ background: '#0B0E11' }}
            >
              <span
                className="font-mono font-bold"
                style={{ color: '#EAECEF' }}
              >
                {action.symbol}
              </span>
              <span
                className="px-2 py-0.5 rounded text-xs font-bold"
                style={
                  action.action.includes('open')
                    ? {
                        background: 'rgba(96, 165, 250, 0.1)',
                        color: '#60a5fa',
                      }
                    : action.action.includes('close')
                    ? {
                        background: 'rgba(14, 203, 129, 0.1)',
                        color: '#0ECB81',
                      }
                    : action.action === 'wait' || action.action === 'hold'
                    ? {
                        background: 'rgba(132, 142, 156, 0.1)',
                        color: '#848E9C',
                      }
                    : {
                        background: 'rgba(248, 113, 113, 0.1)',
                        color: '#F87171',
                      }
                }
              >
                {action.action}
              </span>
              {action.reasoning && (
                <span
                  className="text-xs"
                  style={{ color: '#848E9C', flex: 1 }}
                >
                  {action.reasoning}
                </span>
              )}
            </div>
          ))}
        </div>
      )}

      {decision.execution_log && decision.execution_log.length > 0 && (
        <div
          className="rounded p-3 text-xs font-mono space-y-1"
          style={{ background: '#0B0E11', border: '1px solid #2B3139' }}
        >
          {decision.execution_log.map((log, index) => (
            <div key={`${log}-${index}`} style={{ color: '#EAECEF' }}>
              {log}
            </div>
          ))}
        </div>
      )}

      {/* 跟单扩展展示：中文字段+数值+路径 */}
      {(decision.provider_type || decision.trace_id || decision.formula) && (
        <div className="mt-3 space-y-2 rounded border border-[#2B3139] bg-[#0B0E11] p-3 text-xs text-[#EAECEF]">
          <div className="font-semibold text-sm">跟单详情</div>

          <div className="flex flex-wrap gap-2 items-center text-[11px]">
            {decision.provider_type && renderBadge(`来源：${decision.provider_type}`, 'info')}
            {decision.trace_id && renderBadge(`trace_id: ${decision.trace_id}`, 'info')}
            {decision.price_source && renderBadge(`价源：${decision.price_source}`, 'info')}
            {decision.min_hit && renderBadge('命中最小成交额', 'ok')}
            {decision.max_hit && renderBadge('命中最大成交额', 'warn')}
            {decision.err_code && renderBadge(`错误码：${decision.err_code}`, 'warn')}
            {decision.copy_skip_reason && renderBadge(`原因：${decision.copy_skip_reason}`, 'warn')}
          </div>

          <div className="rounded border border-[#2B3139] bg-[#0B0E11] p-2">
            <div className="font-semibold mb-1 text-[#EAECEF]">换算结果</div>
            <div className="space-y-1 text-[#C4CFDE]">
              <div>
                领航员成交额 / 净值：{formatNumber(leaderNotional)} / {formatNumber(leaderEquity)}
                {rawRatio !== undefined && ` （原始比例 ${formatNumber(rawRatio, 6)}）`}
              </div>
              <div>
                跟随净值：{formatNumber(followerEquity)}，跟单系数：{formatNumber(copyRatio, 2)}%
                {targetNotional !== undefined && ` → 目标成交额 ${formatNumber(targetNotional)}`}
              </div>
              <div>
                实际下单成交额：{formatNumber(followerNotional)}，数量：{formatNumber(followerQty, 8)}
                {derivedPrice && `（折算价格 ${formatNumber(derivedPrice)}，价源 ${decision.price_source ?? '未记录'}）`}
                {leaderPrice > 0 && `，领航员成交价 ${formatNumber(leaderPrice)}`}
              </div>
              {decision.formula && <div className="text-[#9CA3AF]">公式：{decision.formula}</div>}
            </div>
          </div>

          <div className="rounded border border-[#2B3139] bg-[#0B0E11] p-2 space-y-1 text-[#C4CFDE]">
            <div className="font-semibold text-[#EAECEF]">判定路径</div>
            <div>阈值：最小 {decision.min_hit ? '已命中' : '未命中'}，最大 {decision.max_hit ? '已命中' : '未命中'}</div>
            <div>
              结果：{decision.success ? '下单成功/已记录' : '跳过或失败'}
              {decision.copy_skip_reason && `（${decision.copy_skip_reason}）`}
            </div>
            {(decision.err_code || decision.error_message) && (
              <div className="text-[#F87171]">
                错误码：{decision.err_code || '-'} {decision.error_message ? `（${decision.error_message}）` : ''}
              </div>
            )}
            {decision.skip_reason && <div className="text-[#F87171]">描述：{decision.skip_reason}</div>}
            {decision.err_code === 'unsyncable_order_id' && (
              <div className="text-[#FBBF24]">提示：此订单无交易所ID，不参与状态同步</div>
            )}
          </div>
        </div>
      )}

      {decision.error_message && (
        <div
          className="rounded p-3 mt-3 text-sm"
          style={{
            background: 'rgba(246, 70, 93, 0.1)',
            border: '1px solid rgba(246, 70, 93, 0.4)',
            color: '#F6465D',
          }}
        >
          ❌ {decision.error_message}
        </div>
      )}
    </div>
  )
}
