package trader

import (
    "bytes"
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "log"
    "math"
    "net/http"
    "sort"
    "strconv"
    "strings"
    "time"
)

// OKXTrader OKX 合约交易器（REST 实现）
// 说明：使用 OKX v5 API，支持余额、持仓、开/平仓、杠杆、仓位模式设置等
type OKXTrader struct {
    apiKey        string
    secretKey     string
    passphrase    string
    testnet       bool
    baseURL       string
    httpClient    *http.Client
    isCrossMargin bool // 记录仓位模式（true=全仓，false=逐仓）

    // 简单缓存：合约交易规则（步长）
    instrumentCache map[string]*okxInstrument
}

// NewOKXTrader 创建 OKX 交易器
func NewOKXTrader(apiKey, secretKey, passphrase string, testnet bool) (Trader, error) {
    client := &http.Client{Timeout: 15 * time.Second}
    return &OKXTrader{
        apiKey:          apiKey,
        secretKey:       secretKey,
        passphrase:      passphrase,
        testnet:         testnet,
        baseURL:         "https://www.okx.com",
        httpClient:      client,
        isCrossMargin:   true,
        instrumentCache: make(map[string]*okxInstrument),
    }, nil
}

// ===== OKX 通用结构与工具 =====

// okxResponse 通用响应包装
type okxResponse[T any] struct {
    Code string `json:"code"`
    Msg  string `json:"msg"`
    Data []T    `json:"data"`
}

// 账户余额结构
type okxBalanceDetail struct {
    Ccy      string `json:"ccy"`
    CashBal  string `json:"cashBal"`
    Eq       string `json:"eq"`
    AvailBal string `json:"availBal"`
    Upl      string `json:"upl"`
}
type okxBalanceData struct {
    TotalEq string             `json:"totalEq"`
    Details []okxBalanceDetail `json:"details"`
}

// 持仓结构
type okxPosition struct {
    InstId   string `json:"instId"`
    PosSide  string `json:"posSide"` // long/short（双向模式）
    Pos      string `json:"pos"`     // 合约张数
    AvgPx    string `json:"avgPx"`
    MarkPx   string `json:"markPx"`
    Upl      string `json:"upl"`
    Lever    string `json:"lever"`
    LiqPx    string `json:"liqPx"`
    MgnMode  string `json:"mgnMode"` // cross/isolated
}

// 行情结构
type okxTicker struct {
    InstId string `json:"instId"`
    Last   string `json:"last"`
    AskPx  string `json:"askPx"`
    BidPx  string `json:"bidPx"`
}

// 合约规则结构（步长）
type okxInstrument struct {
    InstId string `json:"instId"`
    LotSz  string `json:"lotSz"`  // 数量步长
    TickSz string `json:"tickSz"` // 价格步长
}

// 待撤单结构
type okxPendingOrder struct {
    InstId string `json:"instId"`
    OrdId  string `json:"ordId"`
}

// 下单返回结构
type okxOrderResp struct {
    OrdId string `json:"ordId"`
}

// 算法单（触发类订单）查询与取消结构
type okxAlgoPending struct {
    InstId string `json:"instId"`
    AlgoId string `json:"algoId"`
}

// 生成 OKX 时间戳（UTC，毫秒）
func okxTimestamp() string {
    return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// 计算签名
func (t *OKXTrader) sign(ts, method, path, body string) string {
    prehash := ts + strings.ToUpper(method) + path + body
    mac := hmac.New(sha256.New, []byte(t.secretKey))
    mac.Write([]byte(prehash))
    return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// 执行带签名的请求
func (t *OKXTrader) doRequest(method, apiPath string, query map[string]string, body interface{}, out interface{}) error {
    // 生成查询串（签名需要包含 ?query）
    q := ""
    if len(query) > 0 {
        keys := make([]string, 0, len(query))
        for k := range query {
            keys = append(keys, k)
        }
        sort.Strings(keys)
        var parts []string
        for _, k := range keys {
            parts = append(parts, fmt.Sprintf("%s=%s", k, query[k]))
        }
        q = "?" + strings.Join(parts, "&")
    }

    var bodyStr string
    var reqBody *bytes.Reader
    if strings.EqualFold(method, http.MethodPost) || strings.EqualFold(method, http.MethodPut) {
        if body != nil {
            b, err := json.Marshal(body)
            if err != nil {
                return fmt.Errorf("序列化请求体失败: %w", err)
            }
            bodyStr = string(b)
            reqBody = bytes.NewReader(b)
        } else {
            bodyStr = ""
            reqBody = bytes.NewReader([]byte(""))
        }
    }

    ts := okxTimestamp()
    pathForSign := apiPath + q
    sign := t.sign(ts, method, pathForSign, bodyStr)

    url := t.baseURL + pathForSign
    req, err := http.NewRequest(method, url, reqBody)
    if err != nil {
        return fmt.Errorf("创建请求失败: %w", err)
    }

    // 设置签名头
    req.Header.Set("OK-ACCESS-KEY", t.apiKey)
    req.Header.Set("OK-ACCESS-SIGN", sign)
    req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
    req.Header.Set("OK-ACCESS-PASSPHRASE", t.passphrase)
    req.Header.Set("Content-Type", "application/json")
    if t.testnet {
        // 模拟盘头（开启模拟交易）
        req.Header.Set("x-simulated-trading", "1")
    }

    resp, err := t.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("请求失败: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return fmt.Errorf("HTTP错误: %s", resp.Status)
    }

    if out == nil {
        return nil
    }
    dec := json.NewDecoder(resp.Body)
    if err := dec.Decode(out); err != nil {
        return fmt.Errorf("解析响应失败: %w", err)
    }
    return nil
}

// 转换 symbol 到 OKX 合约ID，例如 BTCUSDT -> BTC-USDT-SWAP
func (t *OKXTrader) toInstId(symbol string) string {
    base := strings.TrimSuffix(strings.ToUpper(symbol), "USDT")
    return base + "-USDT-SWAP"
}

// 将 OKX 合约ID 转换回统一 symbol，例如 BTC-USDT-SWAP -> BTCUSDT
func (t *OKXTrader) toSymbol(instId string) string {
    parts := strings.Split(instId, "-")
    if len(parts) >= 2 {
        return strings.ToUpper(parts[0] + parts[1])
    }
    return strings.ToUpper(instId)
}

// 获取并缓存合约规则（步长）
func (t *OKXTrader) getInstrument(instId string) (*okxInstrument, error) {
    if inst, ok := t.instrumentCache[instId]; ok {
        return inst, nil
    }
    var resp okxResponse[okxInstrument]
    err := t.doRequest(http.MethodGet, "/api/v5/public/instruments", map[string]string{
        "instType": "SWAP",
    }, nil, &resp)
    if err != nil {
        return nil, err
    }
    for _, it := range resp.Data {
        if it.InstId == instId {
            t.instrumentCache[instId] = &it
            return &it, nil
        }
    }
    return nil, fmt.Errorf("未找到合约规则: %s", instId)
}

// ===== Trader 接口实现 =====

// GetBalance 获取账户余额
func (t *OKXTrader) GetBalance() (map[string]interface{}, error) {
    log.Printf("🔄 正在调用 OKX API 获取账户余额...")
    var resp okxResponse[okxBalanceData]
    err := t.doRequest(http.MethodGet, "/api/v5/account/balance", map[string]string{
        "ccy": "USDT",
    }, nil, &resp)
    if err != nil {
        return nil, fmt.Errorf("获取账户余额失败: %w", err)
    }
    if len(resp.Data) == 0 {
        return nil, fmt.Errorf("账户余额返回为空")
    }

    data := resp.Data[0]
    totalEq, _ := strconv.ParseFloat(data.TotalEq, 64)
    var availBal, upl float64
    var cashBal float64
    for _, d := range data.Details {
        if strings.EqualFold(d.Ccy, "USDT") {
            availBal, _ = strconv.ParseFloat(d.AvailBal, 64)
            upl, _ = strconv.ParseFloat(d.Upl, 64)
            // cashBal 是不含未实现的现金余额
            cashBal, _ = strconv.ParseFloat(d.CashBal, 64)
            break
        }
    }
    // totalEq 已含未实现盈亏，钱包余额（不含未实现）优先使用 cashBal，否则 totalEq-upl
    wallet := cashBal
    if wallet == 0 {
        wallet = totalEq - upl
    }

    result := map[string]interface{}{
        "totalWalletBalance": wallet,
        "availableBalance":   availBal,
        "totalUnrealizedProfit": upl,
    }
    log.Printf("✓ OKX 账户: 总净值=%.4f, 钱包=%.4f, 可用=%.4f, 未实现=%.4f", totalEq, wallet, availBal, upl)
    return result, nil
}

// GetPositions 获取所有持仓
func (t *OKXTrader) GetPositions() ([]map[string]interface{}, error) {
    var resp okxResponse[okxPosition]
    err := t.doRequest(http.MethodGet, "/api/v5/account/positions", map[string]string{
        "instType": "SWAP",
    }, nil, &resp)
    if err != nil {
        return nil, fmt.Errorf("获取持仓失败: %w", err)
    }
    var result []map[string]interface{}
    for _, p := range resp.Data {
        posAmt, _ := strconv.ParseFloat(p.Pos, 64)
        if posAmt == 0 {
            continue
        }
        entryPrice, _ := strconv.ParseFloat(p.AvgPx, 64)
        markPrice, _ := strconv.ParseFloat(p.MarkPx, 64)
        upl, _ := strconv.ParseFloat(p.Upl, 64)
        leverage, _ := strconv.ParseFloat(p.Lever, 64)
        liqPx, _ := strconv.ParseFloat(p.LiqPx, 64)

        m := map[string]interface{}{
            "symbol":           t.toSymbol(p.InstId),
            "positionAmt":      math.Abs(posAmt),
            "entryPrice":       entryPrice,
            "markPrice":        markPrice,
            "unRealizedProfit": upl,
            "leverage":         leverage,
            "liquidationPrice": liqPx,
        }
        // 方向
        if strings.EqualFold(p.PosSide, "long") {
            m["side"] = "long"
        } else {
            m["side"] = "short"
        }
        result = append(result, m)
    }
    return result, nil
}

// SetMarginMode 设置仓位模式（同时设置为双向持仓）
func (t *OKXTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
    instId := t.toInstId(symbol)
    // 1) 设置仓位模式为双向（long_short_mode）
    var posResp okxResponse[struct{}]
    if err := t.doRequest(http.MethodPost, "/api/v5/account/set-position-mode", nil, map[string]string{
        "posMode": "long_short_mode",
    }, &posResp); err != nil {
        log.Printf("  ⚠️ 设置仓位模式失败: %v", err)
    }

    // 2) 记录并尝试用 set-leverage 设置保证金模式（lever=1，不改变杠杆）
    t.isCrossMargin = isCrossMargin
    mode := "cross"
    if !isCrossMargin {
        mode = "isolated"
    }
    var levResp okxResponse[struct{}]
    if err := t.doRequest(http.MethodPost, "/api/v5/account/set-leverage", nil, map[string]string{
        "instId":  instId,
        "lever":   "1",
        "mgnMode": mode,
    }, &levResp); err != nil {
        log.Printf("  ⚠️ 设置保证金模式失败（可能已有持仓无法切换）: %v", err)
        return nil // 不阻塞后续交易
    }
    log.Printf("  ✓ %s 仓位模式已设为 %s（双向持仓）", symbol, map[bool]string{true: "全仓", false: "逐仓"}[isCrossMargin])
    return nil
}

// SetLeverage 设置杠杆
func (t *OKXTrader) SetLeverage(symbol string, leverage int) error {
    instId := t.toInstId(symbol)
    mode := "cross"
    if !t.isCrossMargin {
        mode = "isolated"
    }
    var resp okxResponse[struct{}]
    if err := t.doRequest(http.MethodPost, "/api/v5/account/set-leverage", nil, map[string]string{
        "instId":  instId,
        "lever":   strconv.Itoa(leverage),
        "mgnMode": mode,
    }, &resp); err != nil {
        return fmt.Errorf("设置杠杆失败: %w", err)
    }
    log.Printf("  ✓ %s 杠杆已切换为 %dx（%s）", symbol, leverage, mode)
    return nil
}

// OpenLong 开多仓（市价）
func (t *OKXTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
    // 先取消旧委托（避免止盈止损干扰）
    if err := t.CancelAllOrders(symbol); err != nil {
        log.Printf("  ⚠ 取消旧委托失败: %v", err)
    }
    // 切杠杆
    if err := t.SetLeverage(symbol, leverage); err != nil {
        return nil, err
    }
    // 下单
    instId := t.toInstId(symbol)
    qtyStr, err := t.FormatQuantity(symbol, quantity)
    if err != nil {
        return nil, err
    }
    body := map[string]string{
        "instId":  instId,
        "tdMode":  map[bool]string{true: "cross", false: "isolated"}[t.isCrossMargin],
        "side":    "buy",
        "posSide": "long",
        "ordType": "market",
        "sz":      qtyStr,
    }
    var resp okxResponse[okxOrderResp]
    if err := t.doRequest(http.MethodPost, "/api/v5/trade/order", nil, body, &resp); err != nil {
        return nil, fmt.Errorf("开多仓失败: %w", err)
    }
    ordId := ""
    if len(resp.Data) > 0 {
        ordId = resp.Data[0].OrdId
    }
    return map[string]interface{}{"orderId": ordId, "symbol": symbol, "status": "FILLED"}, nil
}

// OpenShort 开空仓（市价）
func (t *OKXTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
    if err := t.CancelAllOrders(symbol); err != nil {
        log.Printf("  ⚠ 取消旧委托失败: %v", err)
    }
    if err := t.SetLeverage(symbol, leverage); err != nil {
        return nil, err
    }
    instId := t.toInstId(symbol)
    qtyStr, err := t.FormatQuantity(symbol, quantity)
    if err != nil {
        return nil, err
    }
    body := map[string]string{
        "instId":  instId,
        "tdMode":  map[bool]string{true: "cross", false: "isolated"}[t.isCrossMargin],
        "side":    "sell",
        "posSide": "short",
        "ordType": "market",
        "sz":      qtyStr,
    }
    var resp okxResponse[okxOrderResp]
    if err := t.doRequest(http.MethodPost, "/api/v5/trade/order", nil, body, &resp); err != nil {
        return nil, fmt.Errorf("开空仓失败: %w", err)
    }
    ordId := ""
    if len(resp.Data) > 0 {
        ordId = resp.Data[0].OrdId
    }
    return map[string]interface{}{"orderId": ordId, "symbol": symbol, "status": "FILLED"}, nil
}

// CloseLong 平多仓（市价，reduceOnly）
func (t *OKXTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
    // 如果数量为0，则查当前多仓数量
    if quantity == 0 {
        positions, err := t.GetPositions()
        if err != nil {
            return nil, err
        }
        for _, p := range positions {
            if p["symbol"] == symbol && p["side"] == "long" {
                quantity = p["positionAmt"].(float64)
                break
            }
        }
        if quantity == 0 {
            return nil, fmt.Errorf("没有找到 %s 的多仓", symbol)
        }
    }
    instId := t.toInstId(symbol)
    qtyStr, err := t.FormatQuantity(symbol, quantity)
    if err != nil {
        return nil, err
    }
    body := map[string]string{
        "instId":     instId,
        "tdMode":     map[bool]string{true: "cross", false: "isolated"}[t.isCrossMargin],
        "side":       "sell",
        "posSide":    "long",
        "ordType":    "market",
        "sz":         qtyStr,
        "reduceOnly": "true",
    }
    var resp okxResponse[okxOrderResp]
    if err := t.doRequest(http.MethodPost, "/api/v5/trade/order", nil, body, &resp); err != nil {
        return nil, fmt.Errorf("平多仓失败: %w", err)
    }
    if err := t.CancelAllOrders(symbol); err != nil {
        log.Printf("  ⚠ 平仓后取消挂单失败: %v", err)
    }
    ordId := ""
    if len(resp.Data) > 0 {
        ordId = resp.Data[0].OrdId
    }
    return map[string]interface{}{"orderId": ordId, "symbol": symbol, "status": "FILLED"}, nil
}

// CloseShort 平空仓（市价，reduceOnly）
func (t *OKXTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
    if quantity == 0 {
        positions, err := t.GetPositions()
        if err != nil {
            return nil, err
        }
        for _, p := range positions {
            if p["symbol"] == symbol && p["side"] == "short" {
                quantity = p["positionAmt"].(float64)
                break
            }
        }
        if quantity == 0 {
            return nil, fmt.Errorf("没有找到 %s 的空仓", symbol)
        }
    }
    instId := t.toInstId(symbol)
    qtyStr, err := t.FormatQuantity(symbol, quantity)
    if err != nil {
        return nil, err
    }
    body := map[string]string{
        "instId":     instId,
        "tdMode":     map[bool]string{true: "cross", false: "isolated"}[t.isCrossMargin],
        "side":       "buy",
        "posSide":    "short",
        "ordType":    "market",
        "sz":         qtyStr,
        "reduceOnly": "true",
    }
    var resp okxResponse[okxOrderResp]
    if err := t.doRequest(http.MethodPost, "/api/v5/trade/order", nil, body, &resp); err != nil {
        return nil, fmt.Errorf("平空仓失败: %w", err)
    }
    if err := t.CancelAllOrders(symbol); err != nil {
        log.Printf("  ⚠ 平仓后取消挂单失败: %v", err)
    }
    ordId := ""
    if len(resp.Data) > 0 {
        ordId = resp.Data[0].OrdId
    }
    return map[string]interface{}{"orderId": ordId, "symbol": symbol, "status": "FILLED"}, nil
}

// CancelAllOrders 取消该币种的所有挂单
func (t *OKXTrader) CancelAllOrders(symbol string) error {
    instId := t.toInstId(symbol)
    var resp okxResponse[okxPendingOrder]
    if err := t.doRequest(http.MethodGet, "/api/v5/trade/orders-pending", map[string]string{
        "instType": "SWAP",
        "instId":   instId,
    }, nil, &resp); err != nil {
        return fmt.Errorf("获取挂单失败: %w", err)
    }
    for _, od := range resp.Data {
        var cancelResp okxResponse[struct{}]
        if err := t.doRequest(http.MethodPost, "/api/v5/trade/cancel-order", nil, map[string]string{
            "instId": instId,
            "ordId":  od.OrdId,
        }, &cancelResp); err != nil {
            log.Printf("  ⚠ 取消订单失败 ordId=%s: %v", od.OrdId, err)
        }
    }
    // 取消算法单（触发类订单）
    var algoResp okxResponse[okxAlgoPending]
    if err := t.doRequest(http.MethodGet, "/api/v5/trade/orders-algo-pending", map[string]string{
        "instType": "SWAP",
        "instId":   instId,
    }, nil, &algoResp); err == nil {
        for _, a := range algoResp.Data {
            var cancelAlgo okxResponse[struct{}]
            if err := t.doRequest(http.MethodPost, "/api/v5/trade/cancel-algos", nil, map[string]string{
                "instId": instId,
                "algoId": a.AlgoId,
            }, &cancelAlgo); err != nil {
                log.Printf("  ⚠ 取消算法单失败 algoId=%s: %v", a.AlgoId, err)
            }
        }
    } else {
        log.Printf("  ⚠ 获取算法单失败: %v", err)
    }
    log.Printf("  ✓ 已取消 %s 的所有挂单", symbol)
    return nil
}

// GetMarketPrice 获取市场价格
func (t *OKXTrader) GetMarketPrice(symbol string) (float64, error) {
    instId := t.toInstId(symbol)
    var resp okxResponse[okxTicker]
    if err := t.doRequest(http.MethodGet, "/api/v5/market/ticker", map[string]string{
        "instId": instId,
    }, nil, &resp); err != nil {
        return 0, fmt.Errorf("获取价格失败: %w", err)
    }
    if len(resp.Data) == 0 {
        return 0, fmt.Errorf("未找到 %s 的价格", symbol)
    }
    price, _ := strconv.ParseFloat(resp.Data[0].Last, 64)
    return price, nil
}

// SetStopLoss 设置止损触发单（reduceOnly 市价触发）
func (t *OKXTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
    instId := t.toInstId(symbol)
    qtyStr, err := t.FormatQuantity(symbol, quantity)
    if err != nil {
        return err
    }
    // 方向与持仓侧映射
    side := "sell"
    posSide := "long"
    if strings.EqualFold(positionSide, "SHORT") {
        side = "buy"
        posSide = "short"
    }
    // 价格按 tickSz 对齐
    triggerPx := t.formatPrice(instId, stopPrice)

    // 使用 order-algo 下触发类订单（市价触发，reduceOnly）
    body := map[string]string{
        "instId":     instId,
        "tdMode":     map[bool]string{true: "cross", false: "isolated"}[t.isCrossMargin],
        "side":       side,
        "posSide":    posSide,
        "ordType":    "trigger",
        "sz":         qtyStr,
        "triggerPx":  triggerPx,
        "orderPx":    "-1",
        "reduceOnly": "true",
    }
    var resp okxResponse[struct{ AlgoId string `json:"algoId"` }]
    if err := t.doRequest(http.MethodPost, "/api/v5/trade/order-algo", nil, body, &resp); err != nil {
        return fmt.Errorf("设置止损失败: %w", err)
    }
    log.Printf("  止损单设置成功: %s %s 数量=%s 触发价=%s", symbol, posSide, qtyStr, triggerPx)
    return nil
}

// SetTakeProfit 设置止盈触发单（reduceOnly 市价触发）
func (t *OKXTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
    instId := t.toInstId(symbol)
    qtyStr, err := t.FormatQuantity(symbol, quantity)
    if err != nil {
        return err
    }
    // 方向与持仓侧映射
    side := "sell"
    posSide := "long"
    if strings.EqualFold(positionSide, "SHORT") {
        side = "buy"
        posSide = "short"
    }
    // 价格按 tickSz 对齐
    triggerPx := t.formatPrice(instId, takeProfitPrice)

    body := map[string]string{
        "instId":     instId,
        "tdMode":     map[bool]string{true: "cross", false: "isolated"}[t.isCrossMargin],
        "side":       side,
        "posSide":    posSide,
        "ordType":    "trigger",
        "sz":         qtyStr,
        "triggerPx":  triggerPx,
        "orderPx":    "-1",
        "reduceOnly": "true",
    }
    var resp okxResponse[struct{ AlgoId string `json:"algoId"` }]
    if err := t.doRequest(http.MethodPost, "/api/v5/trade/order-algo", nil, body, &resp); err != nil {
        return fmt.Errorf("设置止盈失败: %w", err)
    }
    log.Printf("  止盈单设置成功: %s %s 数量=%s 触发价=%s", symbol, posSide, qtyStr, triggerPx)
    return nil
}

// FormatQuantity 格式化数量到正确的精度（按 lotSz 步长取整）
func (t *OKXTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
    instId := t.toInstId(symbol)
    inst, err := t.getInstrument(instId)
    if err != nil {
        // 兜底：无规则时按4位小数
        return fmt.Sprintf("%.4f", quantity), nil
    }
    step, _ := strconv.ParseFloat(inst.LotSz, 64)
    if step <= 0 {
        return fmt.Sprintf("%.4f", quantity), nil
    }
    // 向步长对齐：round(quantity/step)*step
    q := math.Round(quantity/step) * step
    // 根据 lotSz 推断小数位
    decimals := 0
    if strings.Contains(inst.LotSz, ".") {
        decimals = len(strings.Split(inst.LotSz, ".")[1])
    }
    format := fmt.Sprintf("%%.%df", decimals)
    // 去除末尾无用0
    s := fmt.Sprintf(format, q)
    s = strings.TrimRight(s, "0")
    s = strings.TrimRight(s, ".")
    if s == "" {
        s = "0"
    }
    return s, nil
}

// 将价格按 tickSz 步长对齐，并返回格式化字符串
func (t *OKXTrader) formatPrice(instId string, price float64) string {
    inst, err := t.getInstrument(instId)
    if err != nil || inst == nil {
        // 兜底 4位小数
        s := fmt.Sprintf("%.4f", price)
        s = strings.TrimRight(s, "0")
        s = strings.TrimRight(s, ".")
        return s
    }
    tick, _ := strconv.ParseFloat(inst.TickSz, 64)
    if tick <= 0 {
        s := fmt.Sprintf("%.4f", price)
        s = strings.TrimRight(s, "0")
        s = strings.TrimRight(s, ".")
        return s
    }
    p := math.Round(price/tick) * tick
    decimals := 0
    if strings.Contains(inst.TickSz, ".") {
        decimals = len(strings.Split(inst.TickSz, ".")[1])
    }
    format := fmt.Sprintf("%%.%df", decimals)
    s := fmt.Sprintf(format, p)
    s = strings.TrimRight(s, "0")
    s = strings.TrimRight(s, ".")
    if s == "" {
        s = "0"
    }
    return s
}