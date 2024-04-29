package scrapers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/diadata-org/diadata/pkg/dia"
	alephiumhelper "github.com/diadata-org/diadata/pkg/dia/helpers/alephium-helper"
	models "github.com/diadata-org/diadata/pkg/model"
	"github.com/diadata-org/diadata/pkg/utils"
	"github.com/sirupsen/logrus"
)

const (
	defaultRefreshDelay              = 400  // millisec
	defaultSleepBetweenContractCalls = 1000 // millisec
	defaultEventsLimit               = 10
	defaultSwapContractsLimit        = 100
)

type AlephiumScraper struct {
	logger *logrus.Entry
	// signaling channels
	shutdown     chan nothing
	shutdownDone chan nothing
	// error handling; to read error or closed, first acquire read lock
	// only cleanup method should hold write lock
	errorLock                 sync.RWMutex
	error                     error
	closed                    bool
	pairScrapers              map[string]*AlephiumPairScraper // pc.ExchangePair -> pairScraperSet
	api                       *alephiumhelper.AlephiumClient
	ticker                    *time.Ticker
	exchangeName              string
	blockchain                string
	eventsLimit               int
	swapContractsLimit        int
	targetSwapContract        string
	chanTrades                chan *dia.Trade
	db                        *models.RelDB
	refreshDelay              time.Duration
	sleepBetweenContractCalls time.Duration
}

func getTimeDurationFromIntAsMilliseconds(input int) time.Duration {
	result := time.Duration(input) * time.Millisecond
	return result
}

// NewAlephiumScraper returns a new AlephiumScraper initialized with default values.
// The instance is asynchronously scraping as soon as it is created.
// ENV values:
//
//		AYIN_REFRESH_DELAY - (optional,millisecond) refresh data after each poll, default "defaultRefreshDelay" value
//	 	AYIN_SLEEP_TIMEOUT - (optional,millisecond), make timeout between API calls, default "defaultSleepBetweenContractCalls" value
//		AYIN_SWAP_CONTRACTS_LIMIT - (optional, int), limit to get swap contact addresses, default "defaultSwapContractsLimit" value
//		AYIN_TARGET_SWAP_CONTRACT - (optional, string), default = ""
//		AYIN_DEBUG - (optional, bool), make stdout output with alephium client http call, default = false
func NewAlephiumScraper(exchange dia.Exchange, scrape bool, relDB *models.RelDB) *AlephiumScraper {
	ayinRefreshDelay := getTimeDurationFromIntAsMilliseconds(utils.GetenvInt(strings.ToUpper(exchange.Name)+"_REFRESH_DELAY", defaultRefreshDelay))
	sleepBetweenContractCalls := getTimeDurationFromIntAsMilliseconds(utils.GetenvInt(strings.ToUpper(exchange.Name)+"_SLEEP_TIMEOUT", defaultSleepBetweenContractCalls))
	isDebug := utils.GetenvBool(strings.ToUpper(exchange.Name)+"_DEBUG", false)
	eventsLimit := utils.GetenvInt(strings.ToUpper(exchange.Name)+"_REFRESH_DELAY", defaultEventsLimit)
	swapContractsLimit := utils.GetenvInt(strings.ToUpper(exchange.Name)+"_SWAP_CONTRACTS_LIMIT", defaultSwapContractsLimit)
	targetSwapContract := utils.Getenv(strings.ToUpper(exchange.Name)+"_TARGET_SWAP_CONTRACT", "")

	alephiumClient := alephiumhelper.NewAlephiumClient(
		log.WithContext(context.Background()).WithField("context", "AlephiumClient"),
		isDebug,
	)
	s := &AlephiumScraper{
		shutdown:                  make(chan nothing),
		shutdownDone:              make(chan nothing),
		pairScrapers:              make(map[string]*AlephiumPairScraper),
		api:                       alephiumClient,
		ticker:                    time.NewTicker(ayinRefreshDelay),
		exchangeName:              exchange.Name,
		blockchain:                exchange.BlockChain.Name,
		error:                     nil,
		chanTrades:                make(chan *dia.Trade),
		db:                        relDB,
		refreshDelay:              ayinRefreshDelay,
		sleepBetweenContractCalls: sleepBetweenContractCalls,
		logger:                    logrus.New().WithContext(context.Background()).WithField("context", "AlephiumScraper"),
		eventsLimit:               eventsLimit,
		swapContractsLimit:        swapContractsLimit,
		targetSwapContract:        targetSwapContract,
	}
	if scrape {
		go s.mainLoop()
	}
	return s
}

// mainLoop runs in a goroutine until channel s is closed.
func (s *AlephiumScraper) mainLoop() {
	err := s.Update()
	if err != nil {
		s.logger.Error(err)
	}
	for {
		select {
		case <-s.ticker.C:
			err := s.Update()
			if err != nil {
				s.logger.Error(err)
			}
		case <-s.shutdown: // user requested shutdown
			s.logger.Println("shutting down")
			s.cleanup(nil)
			return
		}
	}
}

func (s *AlephiumScraper) FillSymbolData(symbol string) (dia.Asset, error) {
	return dia.Asset{Symbol: symbol}, nil
}

func (s *AlephiumScraper) NormalizePair(pair dia.ExchangePair) (dia.ExchangePair, error) {
	return pair, nil
}

func (s *AlephiumScraper) ScrapePair(pair dia.ExchangePair) (PairScraper, error) {
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	if s.error != nil {
		return nil, s.error
	}
	if s.closed {
		return nil, errors.New("AlephiumScraper: Call ScrapePair on closed scraper")
	}
	ps := &AlephiumPairScraper{
		parent:     s,
		pair:       pair,
		lastRecord: 0,
	}

	s.pairScrapers[pair.Symbol] = ps

	return ps, nil
}

func (s *AlephiumScraper) getRowsForTargetSwapContract() ([]dia.SwapRelationWithAssets, error) {
	swapRows, err := s.db.GetSwapRelationsByBlockchain(s.blockchain)

	if err != nil {
		return swapRows, err
	}
	for _, swapRow := range swapRows {
		if s.targetSwapContract != "" && swapRow.ParentAddress == s.targetSwapContract {
			returnedValue := make([]dia.SwapRelationWithAssets, 1)
			returnedValue[0] = swapRow
			return returnedValue, nil
		}
	}
	return swapRows, nil
}

func (s *AlephiumScraper) Update() error {
	logger := s.logger.WithFields(logrus.Fields{
		"function": "Update",
	})

	swapRows, err := s.getRowsForTargetSwapContract()

	if err != nil {
		logger.
			WithError(err).
			Error("failed to GetSwapRelationsByBlockchain")
		return err
	}
	for _, swapRow := range swapRows {
		polling := dia.Polling{
			Blockchain:      s.blockchain,
			ContractAddress: swapRow.ParentAddress,
			Page:            1,
		}
		err := s.db.SetPolling(polling)
		if err != nil {
			logger.
				WithError(err).
				Error("failed to SetPolling")
			continue
		}
		polling, err = s.db.GetPolling(swapRow.ParentAddress, s.blockchain)
		if err != nil {
			logger.
				WithError(err).
				Error("failed to GetPolling")
			continue
		}

		events, err := s.api.GetSwapContractEvents(
			swapRow.ParentAddress,
			s.eventsLimit,
			polling.Page,
		)

		if err != nil {
			return err
		}

		if len(events) == 0 {
			logger.
				Info("empty events. Skip to next iteration...")
			continue
		}
		polling.Page += 1

		_, err = s.db.UpdateNextStartInPolling(polling.ContractAddress, polling.Blockchain, polling.Page)
		if err != nil {
			return err
		}
		for _, event := range events {
			logger.WithField("event", event).WithField("polling.Page", polling.Page).Info("event")
			transactionDetails, err := s.api.GetTransactionDetails(event.TxHash)
			if err != nil {
				logger.
					WithError(err).
					Error("failed to GetTransactionDetails")
				continue
			}

			diaTrade := s.handleTrade(&swapRow, &event, transactionDetails.Timestamp)
			logger.WithField("diaTrade", diaTrade).Info("diaTrade")
			s.chanTrades <- diaTrade
		}

		time.Sleep(s.sleepBetweenContractCalls)
	}

	return nil
}

func (s *AlephiumScraper) handleTrade(swapRow *dia.SwapRelationWithAssets, event *alephiumhelper.EventContract, timestamp int64) *dia.Trade {
	var volume, price float64
	var symbolPair string
	var baseToken, quoteToken *dia.Asset
	decimals0 := int64(swapRow.Asset0.Decimals)
	decimals1 := int64(swapRow.Asset1.Decimals)

	if event.Fields[1].Value != "0" {
		// if we are swapping from ALPH(asset0) to USDT(asset1), - default behaviour
		//	then amount0In ((fields[1]) will be the amount for ALPH
		//	and amount1Out (fields[4]) will be the amount for USDT.
		amount0In, _ := utils.StringToFloat64(event.Fields[1].Value, decimals0)
		amount1Out, _ := utils.StringToFloat64(event.Fields[4].Value, decimals1)
		volume = -amount0In
		price = amount1Out / amount0In
		symbolPair = fmt.Sprintf("%s-%s", swapRow.Asset0.Symbol, swapRow.Asset1.Symbol)
		baseToken = &swapRow.Asset0
		quoteToken = &swapRow.Asset1
	} else {
		// If we are swapping from USDT(asset1) to ALPH(asset0),
		//	then amount1In ((fields[2]) will be the amount for USDT
		//	and amount0Out (fields[3]) will be the amount for ALPH.
		amount0In, _ := utils.StringToFloat64(event.Fields[2].Value, decimals1)
		amount1Out, _ := utils.StringToFloat64(event.Fields[3].Value, decimals0)
		volume = -amount0In
		price = amount1Out / amount0In
		symbolPair = fmt.Sprintf("%s-%s", swapRow.Asset1.Symbol, swapRow.Asset0.Symbol)
		baseToken = &swapRow.Asset1
		quoteToken = &swapRow.Asset0
	}

	diaTrade := &dia.Trade{
		Time:           time.UnixMilli(timestamp),
		Symbol:         symbolPair,
		Pair:           symbolPair,
		ForeignTradeID: event.TxHash,
		Source:         s.exchangeName,
		Price:          price,
		Volume:         volume,
		VerifiedPair:   true,
		BaseToken:      *baseToken,
		QuoteToken:     *quoteToken,
	}
	return diaTrade
}

// closes all connected PairScrapers
// must only be called from mainLoop
func (s *AlephiumScraper) cleanup(err error) {

	s.errorLock.Lock()
	defer s.errorLock.Unlock()

	s.ticker.Stop()

	if err != nil {
		s.error = err
	}
	s.closed = true

	close(s.shutdownDone) // signal that shutdown is complete
}

// Close closes any existing API connections, as well as channels of
// PairScrapers from calls to ScrapePair
func (s *AlephiumScraper) Close() error {
	if s.closed {
		return errors.New("AlephiumScraper: Already closed")
	}
	close(s.shutdown)
	<-s.shutdownDone
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

// Channel returns a channel that can be used to receive trades/pricing information
func (s *AlephiumScraper) Channel() chan *dia.Trade {
	return s.chanTrades
}

// FetchAvailablePairs returns a list with all available trade pairs as dia.ExchangePair for the pairDiscorvery service
func (s *AlephiumScraper) FetchAvailablePairs() (pairs []dia.ExchangePair, err error) {
	logger := s.logger.WithFields(logrus.Fields{
		"function": "FetchAvailablePairs",
	})
	contractAddresses, err := s.api.GetSwapPairsContractAddresses(s.swapContractsLimit)
	if err != nil {
		logger.WithError(err).Error("failed to get swap contract addresses")
		return
	}
	for _, contractAddress := range contractAddresses.SubContracts {
		tokenPairs, err := s.api.GetTokenPairAddresses(contractAddress)

		if err != nil {
			logger.
				WithField("contractAddress", contractAddress).
				WithError(err).
				Error("failed to get GetTokenPairAddresses")
			continue
		}

		token0, err := s.api.GetTokenInfoForContractDecoded(tokenPairs[0], s.blockchain)
		if err != nil {
			logger.
				WithField("contractAddress", contractAddress).
				WithError(err).
				Error("failed to get GetTokenInfoForContractDecoded for token0")
			continue
		}

		token1, err := s.api.GetTokenInfoForContractDecoded(tokenPairs[1], s.blockchain)
		if err != nil {
			logger.
				WithField("contractAddress", contractAddress).
				WithError(err).
				Error("failed to get GetTokenInfoForContractDecoded for token1")
			continue
		}
		pair := dia.ExchangePair{
			Symbol:      token0.Symbol,
			ForeignName: fmt.Sprintf("%s-%s", token0.Symbol, token1.Symbol),
			Exchange:    s.exchangeName,
		}

		pairs = append(pairs, pair)

		time.Sleep(s.sleepBetweenContractCalls)
	}
	return pairs, nil
}

type AlephiumPairScraper struct {
	parent     *AlephiumScraper
	pair       dia.ExchangePair
	closed     bool
	lastRecord int64
}

func (ps *AlephiumPairScraper) Pair() dia.ExchangePair {
	return ps.pair
}

func (ps *AlephiumPairScraper) Close() error {
	ps.closed = true
	return nil
}

// Error returns an error when the channel Channel() is closed
// and nil otherwise
func (ps *AlephiumPairScraper) Error() error {
	s := ps.parent
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}
