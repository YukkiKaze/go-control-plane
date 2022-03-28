package sotw

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

// process handles a bi-di stream request
func (s *server) processADS(sw *streamWrapper, reqCh chan *discovery.DiscoveryRequest, defaultTypeURL string) error {
	// We make a responder channel here so we can multiplex responses from the dynamic channels.
	sw.watches.addWatch(resource.AnyType, &watch{
		cancel: nil,
		nonce:  "",
		// Create a buffered channel the size of the known resource types.
		response: make(chan cache.Response, types.UnknownType),
	})

	process := func(resp cache.Response) error {
		nonce, err := sw.send(resp)
		if err != nil {
			return err
		}

		sw.watches.responders[resp.GetRequest().TypeUrl].nonce = nonce
		return nil
	}

	processAllExcept := func(typeURL string) error {
		for {
			select {
			// We watch the multiplexed ADS channel for incoming responses.
			case res := <-sw.watches.responders[resource.AnyType].response:
				if res.GetRequest().TypeUrl != typeURL {
					if err := process(res); err != nil {
						return err
					}
				}
			default:
				return nil
			}
		}
	}

	// This control loop strictly orders resources when running in ADS mode.
	// It should be treated as a child process of the original process() loop
	// and should return on close of stream or error.
	// This will cause the cleanup routines in the parent process() loop to execute.
	for {
		select {
		case <-s.ctx.Done():
			return nil
		case req, ok := <-reqCh:
			// Input stream ended or failed.
			if !ok {
				return nil
			}

			// Received an empty request over the request channel. Can't respond.
			if req == nil {
				return status.Errorf(codes.Unavailable, "empty request")
			}

			// Only first request is guaranteed to hold node info so if it's missing, reassign.
			if req.Node != nil {
				sw.node = req.Node
			} else {
				req.Node = sw.node
			}

			// Nonces can be reused across streams; we verify nonce only if nonce is not initialized.
			nonce := req.GetResponseNonce()

			// type URL is required for ADS but is implicit for xDS
			if defaultTypeURL == resource.AnyType {
				if req.TypeUrl == "" {
					return status.Errorf(codes.InvalidArgument, "type URL is required for ADS")
				}
			} else if req.TypeUrl == "" {
				req.TypeUrl = defaultTypeURL
			}

			if s.callbacks != nil {
				if err := s.callbacks.OnStreamRequest(sw.ID, req); err != nil {
					return err
				}
			}

			if lastResponse, ok := sw.lastDiscoveryResponses[req.TypeUrl]; ok {
				if lastResponse.nonce == "" || lastResponse.nonce == nonce {
					// Let's record Resource names that a client has received.
					sw.streamState.SetKnownResourceNames(req.TypeUrl, lastResponse.resources)
				}
			}

			typeURL := req.GetTypeUrl()
			// Use the multiplexed channel for new watches.
			responder := sw.watches.responders[resource.AnyType].response
			if w, ok := sw.watches.responders[typeURL]; ok {
				// We've found a pre-existing watch, lets check and update if needed.
				// If these requirements aren't satisfied, leave an open watch.
				if w.nonce == "" || w.nonce == nonce {
					w.close()

					// Only process if we have an existing watch otherwise go ahead and create.
					if err := processAllExcept(typeURL); err != nil {
						return err
					}

					sw.watches.addWatch(typeURL, &watch{
						cancel:   s.cache.CreateWatch(req, sw.streamState, responder),
						response: responder,
					})
				}
			} else {
				// No pre-existing watch exists, let's create one.
				// We need to precompute the watches first then open a watch in the cache.
				sw.watches.addWatch(typeURL, &watch{
					cancel:   s.cache.CreateWatch(req, sw.streamState, responder),
					response: responder,
				})
			}

		// We only watch the multiplexed channel since all values will come through from process.
		case res := <-sw.watches.responders[resource.AnyType].response:
			err := process(res)
			if err != nil {
				return status.Errorf(codes.Unavailable, err.Error())
			}
		}
	}
}