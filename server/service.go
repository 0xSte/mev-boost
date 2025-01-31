package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	builderApi "github.com/attestantio/go-builder-client/api"
	builderApiV1 "github.com/attestantio/go-builder-client/api/v1"
	eth2ApiV1Deneb "github.com/attestantio/go-eth2-client/api/v1/deneb"
	eth2ApiV1Electra "github.com/attestantio/go-eth2-client/api/v1/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/flashbots/go-boost-utils/ssz"
	"github.com/flashbots/go-utils/httplogger"
	"github.com/flashbots/mev-boost/config"
	"github.com/flashbots/mev-boost/server/params"
	"github.com/flashbots/mev-boost/server/types"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

var (
	errNoRelays                  = errors.New("no relays")
	errInvalidSlot               = errors.New("invalid slot")
	errInvalidHash               = errors.New("invalid hash")
	errInvalidPubkey             = errors.New("invalid pubkey")
	errNoSuccessfulRelayResponse = errors.New("no successful relay response")
	errServerAlreadyRunning      = errors.New("server already running")
	errNilPrometheusRegistry     = errors.New("nil prometheus registry")
)

var (
	nilHash     = phase0.Hash32{}
	nilResponse = struct{}{}
)

type httpErrorResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type slotUID struct {
	slot uint64
	uid  uuid.UUID
}

// BoostServiceOpts provides all available options for use with NewBoostService
type BoostServiceOpts struct {
	Log                   *logrus.Entry
	ListenAddr            string
	Relays                []types.RelayEntry
	RelayMonitors         []*url.URL
	GenesisForkVersionHex string
	GenesisTime           uint64
	RelayCheck            bool
	RelayMinBid           types.U256Str

	RequestTimeoutGetHeader  time.Duration
	RequestTimeoutGetPayload time.Duration
	RequestTimeoutRegVal     time.Duration
	RequestMaxRetries        int
	PrometheusPort           int
	PrometheusRegistry       *prometheus.Registry
}

// BoostService - the mev-boost service
type BoostService struct {
	listenAddr    string
	relays        []types.RelayEntry
	relayMonitors []*url.URL
	log           *logrus.Entry
	srv           *http.Server
	relayCheck    bool
	relayMinBid   types.U256Str
	genesisTime   uint64

	builderSigningDomain phase0.Domain
	httpClientGetHeader  http.Client
	httpClientGetPayload http.Client
	httpClientRegVal     http.Client
	requestMaxRetries    int

	bids     map[string]bidResp // keeping track of bids, to log the originating relay on withholding
	bidsLock sync.Mutex

	slotUID     *slotUID
	slotUIDLock sync.Mutex

	prometheusPort     int
	prometheusRegistry *prometheus.Registry
}

// NewBoostService created a new BoostService
func NewBoostService(opts BoostServiceOpts) (*BoostService, error) {
	if len(opts.Relays) == 0 {
		return nil, errNoRelays
	}

	builderSigningDomain, err := ComputeDomain(ssz.DomainTypeAppBuilder, opts.GenesisForkVersionHex, phase0.Root{}.String())
	if err != nil {
		return nil, err
	}

	return &BoostService{
		listenAddr:    opts.ListenAddr,
		relays:        opts.Relays,
		relayMonitors: opts.RelayMonitors,
		log:           opts.Log,
		relayCheck:    opts.RelayCheck,
		relayMinBid:   opts.RelayMinBid,
		genesisTime:   opts.GenesisTime,
		bids:          make(map[string]bidResp),
		slotUID:       &slotUID{},

		builderSigningDomain: builderSigningDomain,
		httpClientGetHeader: http.Client{
			Timeout:       opts.RequestTimeoutGetHeader,
			CheckRedirect: httpClientDisallowRedirects,
		},
		httpClientGetPayload: http.Client{
			Timeout:       opts.RequestTimeoutGetPayload,
			CheckRedirect: httpClientDisallowRedirects,
		},
		httpClientRegVal: http.Client{
			Timeout:       opts.RequestTimeoutRegVal,
			CheckRedirect: httpClientDisallowRedirects,
		},
		requestMaxRetries:  opts.RequestMaxRetries,
		prometheusPort:     opts.PrometheusPort,
		prometheusRegistry: opts.PrometheusRegistry,
	}, nil
}

func (m *BoostService) respondError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := httpErrorResp{code, message}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		m.log.WithField("response", resp).WithError(err).Error("Couldn't write error response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (m *BoostService) respondOK(w http.ResponseWriter, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		m.log.WithField("response", response).WithError(err).Error("Couldn't write OK response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (m *BoostService) getRouter() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/", m.handleRoot)

	r.HandleFunc(params.PathStatus, m.handleStatus).Methods(http.MethodGet)
	r.HandleFunc(params.PathRegisterValidator, m.handleRegisterValidator).Methods(http.MethodPost)
	r.HandleFunc(params.PathGetHeader, m.handleGetHeader).Methods(http.MethodGet)
	r.HandleFunc(params.PathGetPayload, m.handleGetPayload).Methods(http.MethodPost)

	r.Use(mux.CORSMethodMiddleware(r))
	loggedRouter := httplogger.LoggingMiddlewareLogrus(m.log, r)
	return loggedRouter
}

// StartHTTPServer starts the HTTP server for this boost service instance
func (m *BoostService) StartHTTPServer() error {
	if m.srv != nil {
		return errServerAlreadyRunning
	}

	go m.startBidCacheCleanupTask()

	m.srv = &http.Server{
		Addr:    m.listenAddr,
		Handler: m.getRouter(),

		ReadTimeout:       time.Duration(config.ServerReadTimeoutMs) * time.Millisecond,
		ReadHeaderTimeout: time.Duration(config.ServerReadHeaderTimeoutMs) * time.Millisecond,
		WriteTimeout:      time.Duration(config.ServerWriteTimeoutMs) * time.Millisecond,
		IdleTimeout:       time.Duration(config.ServerIdleTimeoutMs) * time.Millisecond,

		MaxHeaderBytes: config.ServerMaxHeaderBytes,
	}

	err := m.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// StartMetricsServer starts the HTTP server for exporting open-metrics
func (m *BoostService) StartMetricsServer() error {
	if m.prometheusRegistry == nil {
		return errNilPrometheusRegistry
	}
	serveMux := http.NewServeMux()
	serveMux.Handle("/metrics", promhttp.HandlerFor(m.prometheusRegistry, promhttp.HandlerOpts{
		ErrorLog:          m.log,
		EnableOpenMetrics: true,
	}))
	return http.ListenAndServe(
		fmt.Sprintf(":%d", m.prometheusPort),
		serveMux,
	)
}

func (m *BoostService) startBidCacheCleanupTask() {
	for {
		time.Sleep(1 * time.Minute)
		m.bidsLock.Lock()
		for k, bidResp := range m.bids {
			if time.Since(bidResp.t) > 3*time.Minute {
				delete(m.bids, k)
			}
		}
		m.bidsLock.Unlock()
	}
}

func (m *BoostService) sendValidatorRegistrationsToRelayMonitors(payload []builderApiV1.SignedValidatorRegistration) {
	log := m.log.WithField("method", "sendValidatorRegistrationsToRelayMonitors").WithField("numRegistrations", len(payload))
	for _, relayMonitor := range m.relayMonitors {
		go func(relayMonitor *url.URL) {
			url := types.GetURI(relayMonitor, params.PathRegisterValidator)
			log = log.WithField("url", url)
			_, err := SendHTTPRequest(context.Background(), m.httpClientRegVal, http.MethodPost, url, "", nil, payload, nil)
			if err != nil {
				log.WithError(err).Warn("error calling registerValidator on relay monitor")
				return
			}
			log.Debug("sent validator registrations to relay monitor")
		}(relayMonitor)
	}
}

func (m *BoostService) handleRoot(w http.ResponseWriter, _ *http.Request) {
	m.respondOK(w, nilResponse)
}

// handleStatus sends calls to the status endpoint of every relay.
// It returns OK if at least one returned OK, and returns error otherwise.
func (m *BoostService) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(HeaderKeyVersion, config.Version)
	if !m.relayCheck || m.CheckRelays() > 0 {
		m.respondOK(w, nilResponse)
	} else {
		m.respondError(w, http.StatusServiceUnavailable, "all relays are unavailable")
	}
}

// handleRegisterValidator - returns 200 if at least one relay returns 200, else 502
func (m *BoostService) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "registerValidator")
	log.Debug("registerValidator")

	payload := []builderApiV1.SignedValidatorRegistration{}
	if err := DecodeJSON(req.Body, &payload); err != nil {
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ua := UserAgent(req.Header.Get("User-Agent"))
	log = log.WithFields(logrus.Fields{
		"numRegistrations": len(payload),
		"ua":               ua,
	})

	// Add request headers
	headers := map[string]string{
		HeaderStartTimeUnixMS: fmt.Sprintf("%d", time.Now().UTC().UnixMilli()),
	}

	relayRespCh := make(chan error, len(m.relays))

	for _, relay := range m.relays {
		go func(relay types.RelayEntry) {
			url := relay.GetURI(params.PathRegisterValidator)
			log := log.WithField("url", url)

			_, err := SendHTTPRequest(context.Background(), m.httpClientRegVal, http.MethodPost, url, ua, headers, payload, nil)
			if err != nil {
				log.WithError(err).Warn("error calling registerValidator on relay")
			}
			relayRespCh <- err
		}(relay)
	}

	go m.sendValidatorRegistrationsToRelayMonitors(payload)

	for i := 0; i < len(m.relays); i++ {
		respErr := <-relayRespCh
		if respErr == nil {
			m.respondOK(w, nilResponse)
			return
		}
	}

	m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
}

// handleGetHeader requests bids from the relays
func (m *BoostService) handleGetHeader(w http.ResponseWriter, req *http.Request) {
	var (
		vars          = mux.Vars(req)
		parentHashHex = vars["parent_hash"]
		pubkey        = vars["pubkey"]
		ua            = UserAgent(req.Header.Get("User-Agent"))
	)

	slot, err := strconv.ParseUint(vars["slot"], 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	log := m.log.WithFields(logrus.Fields{
		"method":     "getHeader",
		"slot":       slot,
		"parentHash": parentHashHex,
		"pubkey":     pubkey,
		"ua":         ua,
	})
	log.Debug("getHeader")

	// Query the relays for the header
	result, err := m.getHeader(log, ua, slot, pubkey, parentHashHex)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	if result.response.IsEmpty() {
		log.Info("no bid received")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Remember the bid, for future logging in case of withholding
	m.bidsLock.Lock()
	m.bids[bidKey(slot, result.bidInfo.blockHash)] = result
	m.bidsLock.Unlock()

	// Log result
	valueEth := weiBigIntToEthBigFloat(result.bidInfo.value.ToBig())
	log.WithFields(logrus.Fields{
		"blockHash":   result.bidInfo.blockHash.String(),
		"blockNumber": result.bidInfo.blockNumber,
		"txRoot":      result.bidInfo.txRoot.String(),
		"value":       valueEth.Text('f', 18),
		"relays":      strings.Join(types.RelayEntriesToStrings(result.relays), ", "),
	}).Info("best bid")

	// Return the bid
	m.respondOK(w, &result.response)
}

func bidKey(slot uint64, blockHash phase0.Hash32) string {
	return fmt.Sprintf("%v%v", slot, blockHash)
}

func (m *BoostService) respondPayload(w http.ResponseWriter, log *logrus.Entry, result *builderApi.VersionedSubmitBlindedBlockResponse, originalBid bidResp) {
	// If no payload has been received from relay, log loudly about withholding!
	if result == nil || getPayloadResponseIsEmpty(result) {
		originRelays := types.RelayEntriesToStrings(originalBid.relays)
		log.WithField("relaysWithBid", strings.Join(originRelays, ", ")).Error("no payload received from relay!")
		m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
		return
	}
	m.respondOK(w, result)
}

func (m *BoostService) handleGetPayload(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload request starts")

	// Read the body first, so we can log it later on error
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.WithError(err).Error("could not read body of request from the beacon node")
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Read user agent for logging
	userAgent := UserAgent(req.Header.Get("User-Agent"))

	// Decode the body now
	payload := new(eth2ApiV1Electra.SignedBlindedBeaconBlock)
	log.Debug("attempting to decode body into Electra payload")
	if err := DecodeJSON(bytes.NewReader(body), payload); err != nil {
		log.Debug("could not decode Electra request payload, attempting to decode body into Deneb payload")
		payload := new(eth2ApiV1Deneb.SignedBlindedBeaconBlock)
		if err := DecodeJSON(bytes.NewReader(body), payload); err != nil {
			log.Debug("could not decode Deneb request payload")
			log.WithError(err).WithField("body", string(body)).Error("could not decode request payload from the beacon-node (signed blinded beacon block)")
			m.respondError(w, http.StatusBadRequest, err.Error())
			return
		}

		result, originalBid := m.processDenebPayload(log, userAgent, payload)
		m.respondPayload(w, log, result, originalBid)
		return
	}
	result, originalBid := m.processElectraPayload(log, userAgent, payload)
	m.respondPayload(w, log, result, originalBid)
}

// CheckRelays sends a request to each one of the relays previously registered to get their status
func (m *BoostService) CheckRelays() int {
	var wg sync.WaitGroup
	var numSuccessRequestsToRelay uint32

	for _, r := range m.relays {
		wg.Add(1)

		go func(relay types.RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(params.PathStatus)
			log := m.log.WithField("url", url)
			log.Debug("checking relay status")

			code, err := SendHTTPRequest(context.Background(), m.httpClientGetHeader, http.MethodGet, url, "", nil, nil, nil)
			if err != nil {
				log.WithError(err).Error("relay status error - request failed")
				return
			}
			if code == http.StatusOK {
				log.Debug("relay status OK")
			} else {
				log.Errorf("relay status error - unexpected status code %d", code)
				return
			}

			// Success: increase counter and cancel all pending requests to other relays
			atomic.AddUint32(&numSuccessRequestsToRelay, 1)
		}(r)
	}

	// At the end, wait for every routine and return status according to relay's ones.
	wg.Wait()
	return int(numSuccessRequestsToRelay)
}
