package service

import (
	"fmt"
	"github.com/assimon/luuu/config"
	"github.com/assimon/luuu/model/dao"
	"github.com/assimon/luuu/model/data"
	"github.com/assimon/luuu/model/mdb"
	"github.com/assimon/luuu/model/request"
	"github.com/assimon/luuu/model/response"
	"github.com/assimon/luuu/mq"
	"github.com/assimon/luuu/mq/handle"
	"github.com/assimon/luuu/util/constant"
	"github.com/assimon/luuu/util/math"
	"github.com/golang-module/carbon/v2"
	"github.com/hibiken/asynq"
	"github.com/shopspring/decimal"
	"math/rand"
	"sync"
	"time"
    "bytes"
    "encoding/json"
    "io/ioutil"
    "net/http"
    "strconv"
)

const (
	CnyMinimumPaymentAmount  = 0.01 // cny最低支付金额
	UsdtMinimumPaymentAmount = 0.01 // usdt最低支付金额
	UsdtAmountPerIncrement   = 0.01 // usdt每次递增金额
	IncrementalMaximumNumber = 100  // 最大递增次数
)

var gCreateTransactionLock sync.Mutex

// CreateTransaction 创建订单
func CreateTransaction(req *request.CreateTransactionRequest) (*response.CreateTransactionResponse, error) {
	gCreateTransactionLock.Lock()
	defer gCreateTransactionLock.Unlock()
	payAmount := math.MustParsePrecFloat64(req.Amount, 2)
	// 按照汇率转化USDT
	decimalPayAmount := decimal.NewFromFloat(payAmount)
	decimalRateDecimal := decimal.NewFromFloat(config.GetUsdtRate())

    url := "https://p2p.binance.com/bapi/c2c/v2/friendly/c2c/adv/search"

	// JSON payload
    payload := map[string]interface{}{
        "fiat":                "CNY",
        "page":                1,
        "rows":                10,
        "transAmount":         req.Amount,
        "tradeType":           "SELL",
        "asset":               "USDT",
        "countries":           []interface{}{},
        "proMerchantAds":      false,
        "shieldMerchantAds":   false,
        "filterType":          "all",
        "periods":             []interface{}{},
        "additionalKycVerifyFilter": 0,
        "publisherType":       nil,
        "payTypes":            []interface{}{},
        "classifies":          []string{"mass", "profession"},
    }

    // Convert payload to JSON
    jsonData, err := json.Marshal(payload)
    if err != nil {
        return nil, fmt.Errorf("error marshaling JSON: %v", err)
    }

    // Create a new HTTP request
    binanceP2pRequest, err2 := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
    if err2 != nil {
        return nil, fmt.Errorf("error creating request: %w", err2)
    }

    binanceP2pRequest.Header.Set("Content-Type", "application/json")

    // Send the request
    client := &http.Client{}
    binanceP2pResp, err := client.Do(binanceP2pRequest)
    if err != nil {
        return nil, fmt.Errorf("error sending request: %w", err)
    }
    defer binanceP2pResp.Body.Close()

    // Read the response body
    body, err := ioutil.ReadAll(binanceP2pResp.Body)
    if err != nil {
        return nil, fmt.Errorf("error reading response body: %w", err)
    }

    // Parse the response body
    var response2 map[string]interface{}
    err = json.Unmarshal(body, &response2)
    if err != nil {
        return nil, fmt.Errorf("error unmarshaling response: %w", err)
    }

    // Check if response.code equals "000000"
    if response2["code"] != "000000" {
        return nil, fmt.Errorf("%s", response2["message"])
    }
	// Access response.data[0].adv.price
	data2 := response2["data"].([]interface{})
	if len(data2) > 0 {
		adv := data2[0].(map[string]interface{})
		priceStr := adv["adv"].(map[string]interface{})["price"].(string)

		// Convert price to float
		price, err := strconv.ParseFloat(priceStr, 64)
		if err == nil {
			decimalRateDecimal = decimal.NewFromFloat(price)
		}
	}

	// 现在可以安全地调用 Div 方法了，因为参数的类型是正确的
	decimalUsdt := decimalPayAmount.Div(decimalRateDecimal)
	// cny 是否可以满足最低支付金额
	if decimalPayAmount.Cmp(decimal.NewFromFloat(CnyMinimumPaymentAmount)) == -1 {
		return nil, constant.PayAmountErr
	}
	// Usdt是否可以满足最低支付金额
	if decimalUsdt.Cmp(decimal.NewFromFloat(UsdtMinimumPaymentAmount)) == -1 {
		return nil, constant.PayAmountErr
	}
	// 已经存在了的交易
	exist, err := data.GetOrderInfoByOrderId(req.OrderId)
	if err != nil {
		return nil, err
	}
	if exist.ID > 0 {
		return nil, constant.OrderAlreadyExists
	}
	// 有无可用钱包
	walletAddress, err := data.GetAvailableWalletAddress()
	if err != nil {
		return nil, err
	}
	if len(walletAddress) <= 0 {
		return nil, constant.NotAvailableWalletAddress
	}
	amount := math.MustParsePrecFloat64(decimalUsdt.InexactFloat64(), 2)
	availableToken, availableAmount, err := CalculateAvailableWalletAndAmount(amount, walletAddress)
	if err != nil {
		return nil, err
	}
	if availableToken == "" {
		return nil, constant.NotAvailableAmountErr
	}
	tx := dao.Mdb.Begin()
	order := &mdb.Orders{
		TradeId:      GenerateCode(),
		OrderId:      req.OrderId,
		Amount:       req.Amount,
		ActualAmount: availableAmount,
		Token:        availableToken,
		Status:       mdb.StatusWaitPay,
		NotifyUrl:    req.NotifyUrl,
		RedirectUrl:  req.RedirectUrl,
	}
	err = data.CreateOrderWithTransaction(tx, order)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	// 锁定支付池
	err = data.LockTransaction(availableToken, order.TradeId, availableAmount, config.GetOrderExpirationTimeDuration())
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	tx.Commit()
	// 超时过期消息队列
	orderExpirationQueue, _ := handle.NewOrderExpirationQueue(order.TradeId)
	mq.MClient.Enqueue(orderExpirationQueue, asynq.ProcessIn(config.GetOrderExpirationTimeDuration()))
	ExpirationTime := carbon.Now().AddMinutes(config.GetOrderExpirationTime()).Timestamp()
	resp := &response.CreateTransactionResponse{
		TradeId:        order.TradeId,
		OrderId:        order.OrderId,
		Amount:         order.Amount,
		ActualAmount:   order.ActualAmount,
		Token:          order.Token,
		ExpirationTime: ExpirationTime,
		PaymentUrl:     fmt.Sprintf("%s/pay/checkout-counter/%s", config.GetAppUri(), order.TradeId),
	}
	return resp, nil
}

// OrderProcessing 成功处理订单
func OrderProcessing(req *request.OrderProcessingRequest) error {
	tx := dao.Mdb.Begin()
	exist, err := data.GetOrderByBlockIdWithTransaction(tx, req.BlockTransactionId)
	if err != nil {
		return err
	}
	if exist.ID > 0 {
		tx.Rollback()
		return constant.OrderBlockAlreadyProcess
	}
	// 标记订单成功
	err = data.OrderSuccessWithTransaction(tx, req)
	if err != nil {
		tx.Rollback()
		return err
	}
	// 解锁交易
	err = data.UnLockTransaction(req.Token, req.Amount)
	if err != nil {
		tx.Rollback()
		return err
	}
	tx.Commit()
	return nil
}

// CalculateAvailableWalletAndAmount 计算可用钱包地址和金额
func CalculateAvailableWalletAndAmount(amount float64, walletAddress []mdb.WalletAddress) (string, float64, error) {
	availableToken := ""
	availableAmount := amount
	calculateAvailableWalletFunc := func(amount float64) (string, error) {
		availableWallet := ""
		for _, address := range walletAddress {
			token := address.Token
			result, err := data.GetTradeIdByWalletAddressAndAmount(token, amount)
			if err != nil {
				return "", err
			}
			if result == "" {
				availableWallet = token
				break
			}
		}
		return availableWallet, nil
	}
	for i := 0; i < IncrementalMaximumNumber; i++ {
		token, err := calculateAvailableWalletFunc(availableAmount)
		if err != nil {
			return "", 0, err
		}
		// 拿不到可用钱包就累加金额
		if token == "" {
			decimalOldAmount := decimal.NewFromFloat(availableAmount)
			decimalIncr := decimal.NewFromFloat(UsdtAmountPerIncrement)
			availableAmount = decimalOldAmount.Add(decimalIncr).InexactFloat64()
			continue
		}
		availableToken = token
		break
	}
	return availableToken, availableAmount, nil
}

// GenerateCode 订单号生成
func GenerateCode() string {
	date := time.Now().Format("20060102")
	r := rand.Intn(1000)
	code := fmt.Sprintf("%s%d%03d", date, time.Now().UnixNano()/1e6, r)
	return code
}

// GetOrderInfoByTradeId 通过交易号获取订单
func GetOrderInfoByTradeId(tradeId string) (*mdb.Orders, error) {
	order, err := data.GetOrderInfoByTradeId(tradeId)
	if err != nil {
		return nil, err
	}
	if order.ID <= 0 {
		return nil, constant.OrderNotExists
	}
	return order, nil
}
