package trader

import (
	"fmt"
	"nofx/logger"
	"nofx/store"
	"sync"
	"time"
	"strings"
)

// OrderSyncManager 订单状态同步管理器
// 负责定期扫描所有 NEW 状态的订单，并更新其状态
type OrderSyncManager struct {
	store        *store.Store
	interval     time.Duration
	stopCh       chan struct{}
	wg           sync.WaitGroup
	traderCache  map[string]Trader // trader_id -> Trader 实例缓存
	configCache  map[string]*store.TraderFullConfig // trader_id -> 配置缓存
	cacheMutex   sync.RWMutex
	retryMutex   sync.Mutex
	retryCount   map[string]int // order_id -> retry 次数
	badIDs       map[string]bool // order_id -> unsyncable
}

// NewOrderSyncManager 创建订单同步管理器
func NewOrderSyncManager(st *store.Store, interval time.Duration) *OrderSyncManager {
	if interval == 0 {
		interval = 10 * time.Second
	}
	return &OrderSyncManager{
		store:       st,
		interval:    interval,
		stopCh:      make(chan struct{}),
		traderCache: make(map[string]Trader),
		configCache: make(map[string]*store.TraderFullConfig),
		retryCount:  make(map[string]int),
		badIDs:      make(map[string]bool),
	}
}

// Start 启动订单同步服务
func (m *OrderSyncManager) Start() {
	m.wg.Add(1)
	go m.run()
	logger.Info("📦 订单同步管理器已启动")
}

// Stop 停止订单同步服务
func (m *OrderSyncManager) Stop() {
	close(m.stopCh)
	m.wg.Wait()

	// 清理缓存
	m.cacheMutex.Lock()
	m.traderCache = make(map[string]Trader)
	m.configCache = make(map[string]*store.TraderFullConfig)
	m.cacheMutex.Unlock()

	logger.Info("📦 订单同步管理器已停止")
}

// run 主循环
func (m *OrderSyncManager) run() {
	defer m.wg.Done()

	// 启动时立即执行一次
	m.syncOrders()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.syncOrders()
		}
	}
}

// WriteOrder 供跟单执行直接写订单记录（trace_id 等）
func (m *OrderSyncManager) WriteOrder(order *store.TraderOrder) {
	if order == nil {
		return
	}
	if order.TraderID == "" || order.OrderID == "" {
		// 没有交易所订单ID时，仍可用 client_order_id/trace_id 做去重；这里简单跳过
	}
	// 避免重复：先查是否存在
	if order.OrderID != "" {
		if existing, _ := m.store.Order().GetByOrderID(order.TraderID, order.OrderID); existing != nil {
			return
		}
	}
	if err := m.store.Order().Create(order); err != nil {
		logger.Infof("⚠️ 写订单记录失败: %v", err)
	}
}

// syncOrders 同步所有待处理订单
func (m *OrderSyncManager) syncOrders() {
	// 获取所有 NEW 状态的订单
	orders, err := m.store.Order().GetAllPendingOrders()
	if err != nil {
		logger.Infof("⚠️  获取待处理订单失败: %v", err)
		return
	}

	if len(orders) == 0 {
		return
	}

	logger.Infof("📦 开始同步 %d 个待处理订单...", len(orders))

	// 按 trader_id 分组
	ordersByTrader := make(map[string][]*store.TraderOrder)
	for _, order := range orders {
		ordersByTrader[order.TraderID] = append(ordersByTrader[order.TraderID], order)
	}

	// 逐个 trader 处理
	for traderID, traderOrders := range ordersByTrader {
		m.syncTraderOrders(traderID, traderOrders)
	}
}

// syncTraderOrders 同步单个 trader 的订单
func (m *OrderSyncManager) syncTraderOrders(traderID string, orders []*store.TraderOrder) {
	// 获取或创建 trader 实例
	trader, err := m.getOrCreateTrader(traderID)
	if err != nil {
		logger.Infof("⚠️  获取 trader 实例失败 (ID: %s): %v", traderID, err)
		return
	}

	for _, order := range orders {
		// 跳过无效/临时 order_id
		if order.ErrCode == "unsyncable_order_id" || strings.HasPrefix(order.OrderID, "tmp-") || m.badIDs[order.OrderID] {
			continue
		}
		m.syncSingleOrder(trader, order)
	}
}

// syncSingleOrder 同步单个订单状态
func (m *OrderSyncManager) syncSingleOrder(trader Trader, order *store.TraderOrder) {
	status, err := trader.GetOrderStatus(order.Symbol, order.OrderID)
	if err != nil {
		m.retryMutex.Lock()
		cnt := m.retryCount[order.OrderID] + 1
		m.retryCount[order.OrderID] = cnt
		m.retryMutex.Unlock()
		// 退避：前 3 次直接跳过，不降级；超过则标 ERROR
		if cnt <= 3 {
			logger.Infof("⚠️  查询订单失败，重试(%d/3) ID=%s err=%v", cnt, order.OrderID, err)
			return
		}
		order.Status = "ERROR"
		order.SkipReason = fmt.Sprintf("query_failed: %v", err)
		order.ErrCode = "status_query_failed"
		if order.PriceSource == "" {
			order.PriceSource = "copy"
		}
		m.badIDs[order.OrderID] = true
		_ = m.store.Order().Update(order)
		return
	}
	// 查询成功清理重试计数
	m.retryMutex.Lock()
	delete(m.retryCount, order.OrderID)
	m.retryMutex.Unlock()

	statusStr, _ := status["status"].(string)

	switch statusStr {
	case "FILLED":
		avgPrice, _ := status["avgPrice"].(float64)
		executedQty, _ := status["executedQty"].(float64)
		commission, _ := status["commission"].(float64)

		// 如果 API 未返回数量，使用原始数量
		if executedQty == 0 {
			executedQty = order.Quantity
		}

		m.markOrderFilled(order, avgPrice, executedQty, commission)

	case "CANCELED", "EXPIRED":
		order.Status = statusStr
		order.ErrCode = "exchange_reject"
		if order.PriceSource == "" {
			order.PriceSource = "copy"
		}
		if err := m.store.Order().Update(order); err != nil {
			logger.Infof("⚠️  更新订单状态失败: %v", err)
		} else {
			logger.Infof("📦 订单状态更新: %s (ID: %s)", statusStr, order.OrderID)
		}
	}
}

// markOrderFilled 标记订单已成交
func (m *OrderSyncManager) markOrderFilled(order *store.TraderOrder, avgPrice, executedQty, commission float64) {
	// 如果 avgPrice 为 0，使用订单价格
	if avgPrice == 0 {
		avgPrice = order.Price
	}
	if executedQty == 0 {
		executedQty = order.Quantity
	}
	if order.PriceSource == "" {
		order.PriceSource = "copy"
	}

	// 计算已实现盈亏（仅平仓订单）
	var realizedPnL float64
	if (order.Action == "close_long" || order.Action == "close_short") && order.EntryPrice > 0 && avgPrice > 0 {
		if order.Action == "close_long" {
			// 平多盈亏 = (平仓价 - 开仓价) * 数量
			realizedPnL = (avgPrice - order.EntryPrice) * executedQty
		} else {
			// 平空盈亏 = (开仓价 - 平仓价) * 数量
			realizedPnL = (order.EntryPrice - avgPrice) * executedQty
		}
	}

	order.AvgPrice = avgPrice
	order.ExecutedQty = executedQty
	order.Status = "FILLED"
	order.Fee = commission
	order.RealizedPnL = realizedPnL
	order.FilledAt = time.Now()

	if err := m.store.Order().Update(order); err != nil {
		logger.Infof("⚠️  更新订单状态失败: %v", err)
	} else {
		if realizedPnL != 0 {
			logger.Infof("✅ 订单已成交 (ID: %s, avgPrice: %.4f, qty: %.4f, PnL: %.2f)",
				order.OrderID, avgPrice, executedQty, realizedPnL)
		} else {
			logger.Infof("✅ 订单已成交 (ID: %s, avgPrice: %.4f, qty: %.4f)",
				order.OrderID, avgPrice, executedQty)
		}
	}
}

// getOrCreateTrader 获取或创建 trader 实例
func (m *OrderSyncManager) getOrCreateTrader(traderID string) (Trader, error) {
	m.cacheMutex.RLock()
	trader, exists := m.traderCache[traderID]
	m.cacheMutex.RUnlock()

	if exists && trader != nil {
		return trader, nil
	}

	// 需要创建新的 trader 实例
	// 首先获取 trader 配置
	config, err := m.getTraderConfig(traderID)
	if err != nil {
		return nil, fmt.Errorf("获取 trader 配置失败: %w", err)
	}

	// 根据交易所类型创建 trader
	trader, err = m.createTrader(config)
	if err != nil {
		return nil, fmt.Errorf("创建 trader 实例失败: %w", err)
	}

	m.cacheMutex.Lock()
	m.traderCache[traderID] = trader
	m.cacheMutex.Unlock()

	return trader, nil
}

// getTraderConfig 获取 trader 配置
func (m *OrderSyncManager) getTraderConfig(traderID string) (*store.TraderFullConfig, error) {
	m.cacheMutex.RLock()
	config, exists := m.configCache[traderID]
	m.cacheMutex.RUnlock()

	if exists {
		return config, nil
	}

	// 从数据库获取 - 需要找到 trader 对应的 userID
	// 首先查询所有 traders 找到对应的 userID
	traders, err := m.store.Trader().ListAll()
	if err != nil {
		return nil, fmt.Errorf("获取 trader 列表失败: %w", err)
	}

	var userID string
	for _, t := range traders {
		if t.ID == traderID {
			userID = t.UserID
			break
		}
	}

	if userID == "" {
		return nil, fmt.Errorf("找不到 trader: %s", traderID)
	}

	config, err = m.store.Trader().GetFullConfig(userID, traderID)
	if err != nil {
		return nil, err
	}

	m.cacheMutex.Lock()
	m.configCache[traderID] = config
	m.cacheMutex.Unlock()

	return config, nil
}

// createTrader 根据配置创建 trader 实例
func (m *OrderSyncManager) createTrader(config *store.TraderFullConfig) (Trader, error) {
	exchange := config.Exchange

	// 使用 exchange.ID 判断具体的交易所，而不是 exchange.Type (cex/dex)
	switch exchange.ID {
	case "binance":
		return NewFuturesTrader(exchange.APIKey, exchange.SecretKey, config.Trader.UserID), nil

	case "bybit":
		return NewBybitTrader(exchange.APIKey, exchange.SecretKey), nil

	case "okx":
		return NewOKXTrader(exchange.APIKey, exchange.SecretKey, exchange.Passphrase), nil

	case "hyperliquid":
		return NewHyperliquidTrader(exchange.SecretKey, exchange.HyperliquidWalletAddr, exchange.Testnet)

	case "aster":
		return NewAsterTrader(exchange.AsterUser, exchange.AsterSigner, exchange.AsterPrivateKey)

	case "lighter":
		if exchange.LighterAPIKeyPrivateKey != "" {
			return NewLighterTraderV2(
				exchange.LighterPrivateKey,
				exchange.LighterWalletAddr,
				exchange.LighterAPIKeyPrivateKey,
				exchange.Testnet,
			)
		}
		return NewLighterTrader(exchange.LighterPrivateKey, exchange.LighterWalletAddr, exchange.Testnet)

	default:
		return nil, fmt.Errorf("不支持的交易所: %s", exchange.ID)
	}
}

// InvalidateCache 使缓存失效（当配置变更时调用）
func (m *OrderSyncManager) InvalidateCache(traderID string) {
	m.cacheMutex.Lock()
	defer m.cacheMutex.Unlock()

	delete(m.traderCache, traderID)
	delete(m.configCache, traderID)
}
