package jwtware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

var ( // ErrKID indicates that the JWT had an invalid kid.
	ErrMissingKeySet = errors.New("not able to download JWKs")

	// ErrKID indicates that the JWT had an invalid kid.
	ErrKID = errors.New("the JWT has an invalid kid")

	// ErrUnsupportedKeyType indicates the JWT key type is an unsupported type.
	ErrUnsupportedKeyType = errors.New("the JWT key type is unsupported")

	// ErrKIDNotFound indicates that the given key ID was not found in the JWKs.
	ErrKIDNotFound = errors.New("the given key ID was not found in the JWKs")

	// ErrMissingAssets indicates there are required assets missing to create a public key.
	ErrMissingAssets = errors.New("required assets are missing to create a public key")
)

// rawJWK represents a raw key inside a JWKs.
type rawJWK struct {
	Curve       string `json:"crv"`
	Exponent    string `json:"e"`
	ID          string `json:"kid"`
	Modulus     string `json:"n"`
	X           string `json:"x"`
	Y           string `json:"y"`
	precomputed interface{}
}

// rawJWKs represents a JWKs in JSON format.
type rawJWKs struct {
	Keys []rawJWK `json:"keys"`
}

// KeySet represents a JSON Web Key Set.
type KeySet struct {
	keys            map[string]*rawJWK
	config          *Config
	cancel          context.CancelFunc
	client          *http.Client
	ctx             context.Context
	mux             sync.RWMutex
	refreshRequests chan context.CancelFunc
}

// keyFunc is a compatibility function that matches the signature of github.com/dgrijalva/jwt-go's keyFunc function.
func (j *KeySet) keyFunc() jwt.Keyfunc {
	return func(token *jwt.Token) (interface{}, error) {
		if j.keys == nil {
			err := j.downloadKeySet()
			if err != nil {
				return nil, fmt.Errorf("%w: key set URL is not accessible", ErrMissingKeySet)
			}
		}

		// Get the kid from the token header.
		kidInter, ok := token.Header["kid"]
		if !ok {
			return nil, fmt.Errorf("%w: could not find kid in JWT header", ErrKID)
		}
		kid, ok := kidInter.(string)
		if !ok {
			return nil, fmt.Errorf("%w: could not convert kid in JWT header to string", ErrKID)
		}

		// Get the JSONKey.
		jsonKey, err := j.getKey(kid)
		if err != nil {
			return nil, err
		}

		// Determine the key's algorithm and return the appropriate public key.
		switch keyAlg := token.Header["alg"]; keyAlg {
		case es256, es384, es512:
			return jsonKey.getECDSA()
		case ps256, ps384, ps512, rs256, rs384, rs512:
			return jsonKey.getRSA()
		default:
			return nil, fmt.Errorf("%w: %s: feel free to add a feature request or contribute to https://github.com/MicahParks/keyfunc", ErrUnsupportedKeyType, keyAlg)
		}
	}
}

// downloadKeySet loads the JWKs at the given URL.
func (j *KeySet) downloadKeySet() (err error) {
	// Apply some defaults if options were not provided.
	if j.client == nil {
		j.client = http.DefaultClient
	}

	// Get the keys for the JWKs.
	if err = j.refresh(); err != nil {
		return err
	}

	// Check to see if a background refresh of the JWKs should happen.
	if j.config.KeyRefreshInterval != nil || j.config.KeyRefreshRateLimit != nil {
		// Attach a context used to end the background goroutine.
		j.ctx, j.cancel = context.WithCancel(context.Background())

		// Create a channel that will accept requests to refresh the JWKs.
		j.refreshRequests = make(chan context.CancelFunc, 1)

		// Start the background goroutine for data refresh.
		go j.startRefreshing()
	}

	return nil
}

// New creates a new JWKs from a raw JSON message.
func parseKeySet(jwksBytes json.RawMessage) (keys map[string]*rawJWK, err error) {
	// Turn the raw JWKs into the correct Go type.
	var rawKS rawJWKs
	if err = json.Unmarshal(jwksBytes, &rawKS); err != nil {
		return nil, err
	}

	// Iterate through the keys in the raw JWKs. Add them to the JWKs.
	keys = make(map[string]*rawJWK, len(rawKS.Keys))
	for _, key := range rawKS.Keys {
		key := key
		keys[key.ID] = &key
	}

	return keys, nil
}

// getKey gets the JSONKey from the given KID from the JWKs. It may refresh the JWKs if configured to.
func (j *KeySet) getKey(kid string) (jsonKey *rawJWK, err error) {
	// Get the JSONKey from the JWKs.
	var ok bool
	j.mux.RLock()
	jsonKey, ok = j.keys[kid]
	j.mux.RUnlock()

	// Check if the key was present.
	if !ok {
		// Check to see if configured to refresh on unknown kid.
		if *j.config.KeyRefreshUnknownKID {
			// Create a context for refreshing the JWKs.
			ctx, cancel := context.WithCancel(j.ctx)

			// Refresh the JWKs.
			select {
			case <-j.ctx.Done():
				return
			case j.refreshRequests <- cancel:
			default:

				// If the j.refreshRequests channel is full, return the error early.
				return nil, ErrKIDNotFound
			}

			// Wait for the JWKs refresh to done.
			<-ctx.Done()

			// Lock the JWKs for async safe use.
			j.mux.RLock()
			defer j.mux.RUnlock()

			// Check if the JWKs refresh contained the requested key.
			if jsonKey, ok = j.keys[kid]; ok {
				return jsonKey, nil
			}
		}

		return nil, ErrKIDNotFound
	}

	return jsonKey, nil
}

// startRefreshing is meant to be a separate goroutine that will update the keys in a JWKs over a given interval of
// time.
func (j *KeySet) startRefreshing() {
	// Create some rate limiting assets.
	var lastRefresh time.Time
	var queueOnce sync.Once
	var refreshMux sync.Mutex
	if j.config.KeyRefreshRateLimit != nil {
		lastRefresh = time.Now().Add(-*j.config.KeyRefreshRateLimit)
	}

	// Create a channel that will never send anything unless there is a refresh interval.
	refreshInterval := make(<-chan time.Time)

	// Enter an infinite loop that ends when the background ends.
	for {
		// If there is a refresh interval, create the channel for it.
		if j.config.KeyRefreshInterval != nil {
			refreshInterval = time.After(*j.config.KeyRefreshInterval)
		}

		// Wait for a refresh to occur or the background to end.
		select {

		// Send a refresh request the JWKs after the given interval.
		case <-refreshInterval:
			select {
			case <-j.ctx.Done():
				return
			case j.refreshRequests <- func() {}:
			default: // If the j.refreshRequests channel is full, don't don't send another request.
			}

		// Accept refresh requests.
		case cancel := <-j.refreshRequests:
			// Rate limit, if needed.
			refreshMux.Lock()
			if j.config.KeyRefreshRateLimit != nil && lastRefresh.Add(*j.config.KeyRefreshRateLimit).After(time.Now()) {
				// Don't make the JWT parsing goroutine wait for the JWKs to refresh.
				cancel()

				// Only queue a refresh once.
				queueOnce.Do(func() {

					// Launch a goroutine that will get a reservation for a JWKs refresh or fail to and immediately return.
					go func() {
						// Wait for the next time to refresh.
						refreshMux.Lock()
						wait := time.Until(lastRefresh.Add(*j.config.KeyRefreshRateLimit))
						refreshMux.Unlock()
						select {
						case <-j.ctx.Done():
							return
						case <-time.After(wait):
						}

						// Refresh the JWKs.
						refreshMux.Lock()
						defer refreshMux.Unlock()
						if err := j.refresh(); err != nil && j.config.KeyRefreshErrorHandler != nil {
							j.config.KeyRefreshErrorHandler(j, err)
						} else if err == nil && j.config.KeyRefreshSuccessHandler != nil {
							j.config.KeyRefreshSuccessHandler(j)
						}

						// Reset the last time for the refresh to now.
						lastRefresh = time.Now()

						// Allow another queue.
						queueOnce = sync.Once{}
					}()
				})
			} else {
				// Refresh the JWKs.
				if err := j.refresh(); err != nil && j.config.KeyRefreshErrorHandler != nil {
					j.config.KeyRefreshErrorHandler(j, err)
				} else if err == nil && j.config.KeyRefreshSuccessHandler != nil {
					j.config.KeyRefreshSuccessHandler(j)
				}

				// Reset the last time for the refresh to now.
				lastRefresh = time.Now()

				// Allow the JWT parsing goroutine to continue with the refreshed JWKs.
				cancel()
			}
			refreshMux.Unlock()

		// Clean up this goroutine when its context expires.
		case <-j.ctx.Done():
			return
		}
	}
}

// refresh does an HTTP GET on the JWKs URL to rebuild the JWKs.
func (j *KeySet) refresh() (err error) {
	// Create a context for the request.
	var ctx context.Context
	var cancel context.CancelFunc
	if j.ctx != nil {
		ctx, cancel = context.WithTimeout(j.ctx, *j.config.KeyRefreshTimeout)
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), *j.config.KeyRefreshTimeout)
	}
	defer cancel()

	// Create the HTTP request.
	var req *http.Request
	if req, err = http.NewRequestWithContext(ctx, http.MethodGet, j.config.KeySetUrl, bytes.NewReader(nil)); err != nil {
		return err
	}

	// Get the JWKs as JSON from the given URL.
	var resp *http.Response
	if resp, err = j.client.Do(req); err != nil {
		return err
	}
	defer resp.Body.Close() // Ignore any error.

	// Read the raw JWKs from the body of the response.
	var jwksBytes []byte
	if jwksBytes, err = ioutil.ReadAll(resp.Body); err != nil {
		return err
	}

	// Create an updated JWKs.
	var keys map[string]*rawJWK
	if keys, err = parseKeySet(jwksBytes); err != nil {
		return err
	}

	// Lock the JWKs for async safe usage.
	j.mux.Lock()
	defer j.mux.Unlock()

	// Update the keys.
	j.keys = keys

	return nil
}

// StopRefreshing ends the background goroutine to update the JWKs. It can only happen once and is only effective if the
// JWKs has a background goroutine refreshing the JWKs keys.
func (j *KeySet) StopRefreshing() {
	if j.cancel != nil {
		j.cancel()
	}
}
