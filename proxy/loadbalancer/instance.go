package loadbalancer

import (
	"context"
	"fmt"
	"m3u-stream-merger/logger"
	"m3u-stream-merger/proxy"
	"m3u-stream-merger/store"
	"m3u-stream-merger/utils"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
)

type LoadBalancerInstance struct {
	Info          store.StreamInfo
	Cm            *store.ConcurrencyManager
	config        *LBConfig
	httpClient    proxy.HTTPClient
	logger        logger.Logger
	indexProvider proxy.IndexProvider
	slugParser    proxy.SlugParser
}

type LoadBalancerInstanceOption func(*LoadBalancerInstance)

func WithHTTPClient(client proxy.HTTPClient) LoadBalancerInstanceOption {
	return func(s *LoadBalancerInstance) {
		s.httpClient = client
	}
}

func WithLogger(logger logger.Logger) LoadBalancerInstanceOption {
	return func(s *LoadBalancerInstance) {
		s.logger = logger
	}
}

func WithIndexProvider(provider proxy.IndexProvider) LoadBalancerInstanceOption {
	return func(s *LoadBalancerInstance) {
		s.indexProvider = provider
	}
}

func WithSlugParser(parser proxy.SlugParser) LoadBalancerInstanceOption {
	return func(s *LoadBalancerInstance) {
		s.slugParser = parser
	}
}

func NewLoadBalancerInstance(
	cm *store.ConcurrencyManager,
	cfg *LBConfig,
	opts ...LoadBalancerInstanceOption,
) *LoadBalancerInstance {
	instance := &LoadBalancerInstance{
		Cm:            cm,
		config:        cfg,
		httpClient:    utils.HTTPClient,
		logger:        &logger.DefaultLogger{},
		indexProvider: &proxy.DefaultIndexProvider{},
		slugParser:    &proxy.DefaultSlugParser{},
	}

	for _, opt := range opts {
		opt(instance)
	}

	return instance
}

type LoadBalancerResult struct {
	Response *http.Response
	URL      string
	Index    string
	SubIndex string
}

func (instance *LoadBalancerInstance) Balance(ctx context.Context, req *http.Request, session *store.Session) (*LoadBalancerResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context cannot be nil")
	}
	if session == nil {
		return nil, fmt.Errorf("session cannot be nil")
	}
	if req == nil {
		return nil, fmt.Errorf("req cannot be nil")
	}
	if req.Method == "" {
		return nil, fmt.Errorf("method cannot be empty")
	}
	if req.URL == nil {
		req.URL = &url.URL{Path: "/"}
	}

	streamUrl := strings.Split(path.Base(req.URL.Path), ".")[0]
	if streamUrl == "" {
		return nil, fmt.Errorf("invalid ID for request from %s: %s", req.RemoteAddr, req.URL.Path)
	}
	streamUrl = strings.TrimPrefix(streamUrl, "/")

	err := instance.fetchBackendUrls(streamUrl)
	if err != nil {
		return nil, fmt.Errorf("error fetching sources for: %s", streamUrl)
	}

	backoff := proxy.NewBackoffStrategy(200*time.Millisecond, 2*time.Second)

	for lap := 0; lap < instance.config.MaxRetries || instance.config.MaxRetries == 0; lap++ {
		instance.logger.Debugf("Stream attempt %d out of %d", lap+1, instance.config.MaxRetries)

		result, err := instance.tryAllStreams(ctx, req.Method, session)
		if err == nil {
			return result, nil
		}
		instance.logger.Debugf("tryAllStreams error: %v", err)

		if err == context.Canceled {
			return nil, fmt.Errorf("cancelling load balancer")
		}

		session.SetTestedIndexes([]string{})

		select {
		case <-time.After(backoff.Next()):
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelling load balancer")
		}
	}

	return nil, fmt.Errorf("error fetching stream: exhausted all streams")
}

func (instance *LoadBalancerInstance) fetchBackendUrls(streamUrl string) error {
	stream, err := instance.slugParser.GetStreamBySlug(streamUrl)
	if err != nil {
		return err
	}

	instance.logger.Debugf("Decoded slug: %v", stream)

	// Validate URLs map
	if len(stream.URLs) == 0 {
		return fmt.Errorf("stream has no URLs configured")
	}

	// Validate that at least one index has URLs
	hasValidUrls := false
	for _, innerMap := range stream.URLs {
		if len(innerMap) > 0 {
			hasValidUrls = true
			break
		}
	}
	if !hasValidUrls {
		return fmt.Errorf("stream has no valid URLs")
	}

	instance.Info = stream

	return nil
}

func (instance *LoadBalancerInstance) tryAllStreams(ctx context.Context, method string, session *store.Session) (*LoadBalancerResult, error) {
	instance.logger.Logf("Trying all stream urls for session: %s", session.ID)

	if instance.indexProvider == nil {
		return nil, fmt.Errorf("index provider cannot be nil")
	}

	m3uIndexes := instance.indexProvider.GetM3UIndexes()
	if len(m3uIndexes) == 0 {
		return nil, fmt.Errorf("no M3U indexes available")
	}

	sort.Slice(m3uIndexes, func(i, j int) bool {
		return instance.Cm.ConcurrencyPriorityValue(m3uIndexes[i]) > instance.Cm.ConcurrencyPriorityValue(m3uIndexes[j])
	})

	select {
	case <-ctx.Done():
		return nil, context.Canceled
	default:
		for _, index := range m3uIndexes {
			innerMap, ok := instance.Info.URLs[index]
			if !ok {
				instance.logger.Errorf("Channel not found from M3U_%s: %s", index, instance.Info.Title)
				continue
			}

			result, err := instance.tryStreamUrls(ctx, method, session, index, innerMap)
			if err == nil {
				return result, nil
			}
		}
	}

	return nil, fmt.Errorf("no available streams")
}

func (instance *LoadBalancerInstance) tryStreamUrls(
	ctx context.Context,
	method string,
	session *store.Session,
	index string,
	urls map[string]string,
) (*LoadBalancerResult, error) {
	if instance.httpClient == nil {
		return nil, fmt.Errorf("HTTP client cannot be nil")
	}

	for subIndex, url := range urls {
		// Thread-safe check of tested indexes
		session.Mutex.RLock()
		alreadyTested := slices.Contains(session.TestedIndexes, index+"|"+subIndex)
		session.Mutex.RUnlock()

		if alreadyTested {
			instance.logger.Debugf("Skipping M3U_%s|%s: marked as previous stream", index, subIndex)
			continue
		}

		if instance.Cm.CheckConcurrency(index) {
			instance.logger.Debugf("Concurrency limit reached for M3U_%s: %s", index, url)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			instance.logger.Errorf("Error creating request: %s", err.Error())
			session.Mutex.Lock()
			session.TestedIndexes = append(session.TestedIndexes, index+"|"+subIndex)
			session.Mutex.Unlock()
			continue
		}

		resp, err := instance.httpClient.Do(req)
		if err != nil {
			instance.logger.Errorf("Error fetching stream: %s", err.Error())
			session.Mutex.Lock()
			session.TestedIndexes = append(session.TestedIndexes, index+"|"+subIndex)
			session.Mutex.Unlock()
			continue
		}

		if resp == nil {
			instance.logger.Errorf("Received nil response from HTTP client")
			session.Mutex.Lock()
			session.TestedIndexes = append(session.TestedIndexes, index+"|"+subIndex)
			session.Mutex.Unlock()
			continue
		}

		if resp.StatusCode != http.StatusOK {
			instance.logger.Errorf("Non-200 status code received: %d for %s %s", resp.StatusCode, method, url)
			session.Mutex.Lock()
			session.TestedIndexes = append(session.TestedIndexes, index+"|"+subIndex)
			session.Mutex.Unlock()
			continue
		}

		instance.logger.Debugf("Successfully fetched stream from %s with method %s", url, method)

		return &LoadBalancerResult{
			Response: resp,
			URL:      url,
			Index:    index,
			SubIndex: subIndex,
		}, nil
	}

	return nil, fmt.Errorf("all urls failed")
}
