package middleware

import (
	"context"
	"errors"
	"net"

	"github.com/chihaya/chihaya/bittorrent"
	"github.com/chihaya/chihaya/storage"
)

// Hook abstracts the concept of anything that needs to interact with a
// BitTorrent client's request and response to a BitTorrent tracker.
type Hook interface {
	HandleAnnounce(context.Context, *bittorrent.AnnounceRequest, *bittorrent.AnnounceResponse) (context.Context, error)
	HandleScrape(context.Context, *bittorrent.ScrapeRequest, *bittorrent.ScrapeResponse) (context.Context, error)
	HandleApi(context.Context, *bittorrent.ApiRequest, *bittorrent.ApiResponse) (context.Context, error)
}

type skipSwarmInteraction struct{}

// SkipSwarmInteractionKey is a key for the context of an Announce to control
// whether the swarm interaction middleware should run.
// Any non-nil value set for this key will cause the swarm interaction
// middleware to skip.
var SkipSwarmInteractionKey = skipSwarmInteraction{}

type swarmInteractionHook struct {
	store storage.PeerStore
}

func (h *swarmInteractionHook) HandleAnnounce(ctx context.Context, req *bittorrent.AnnounceRequest, resp *bittorrent.AnnounceResponse) (_ context.Context, err error) {
	if ctx.Value(SkipSwarmInteractionKey) != nil {
		return ctx, nil
	}

	switch {
	case req.Event == bittorrent.Stopped:
		err = h.store.DeleteSeeder(req.InfoHash, req.Peer)
		if err != nil && err != storage.ErrResourceDoesNotExist {
			return ctx, err
		}

		err = h.store.DeleteLeecher(req.InfoHash, req.Peer)
		if err != nil && err != storage.ErrResourceDoesNotExist {
			return ctx, err
		}
	case req.Event == bittorrent.Completed:
		err = h.store.GraduateLeecher(req.InfoHash, req.Peer)
		return ctx, err
	case req.Left == 0:
		// Completed events will also have Left == 0, but by making this
		// an extra case we can treat "old" seeders differently from
		// graduating leechers. (Calling PutSeeder is probably faster
		// than calling GraduateLeecher.)
		err = h.store.PutSeeder(req.InfoHash, req.Peer)
		return ctx, err
	default:
		err = h.store.PutLeecher(req.InfoHash, req.Peer)
		return ctx, err
	}

	return ctx, nil
}

func (h *swarmInteractionHook) HandleScrape(ctx context.Context, _ *bittorrent.ScrapeRequest, _ *bittorrent.ScrapeResponse) (context.Context, error) {
	// Scrapes have no effect on the swarm.
	return ctx, nil
}

func (h *swarmInteractionHook) HandleApi(ctx context.Context, req *bittorrent.ApiRequest, resp *bittorrent.ApiResponse) (context.Context, error) {
	if req.Method == "delete" {
		for _, infoHash := range req.InfoHashes {
			h.store.DeleteInfoHash(infoHash)
		}
	}

	return ctx, nil
}

// ErrInvalidIP indicates an invalid IP for an Announce.
var ErrInvalidIP = errors.New("invalid IP")

// sanitizationHook enforces semantic assumptions about requests that may have
// not been accounted for in a tracker frontend.
//
// The SanitizationHook performs the following checks:
// - maxNumWant: Checks whether the numWant parameter of an announce is below
//     a limit. Sets it to the limit if the value is higher.
// - defaultNumWant: Checks whether the numWant parameter of an announce is
//     zero. Sets it to the default if it is.
// - IP sanitization: Checks whether the announcing Peer's IP address is either
//     IPv4 or IPv6. Returns ErrInvalidIP if the address is neither IPv4 nor
//     IPv6. Sets the Peer.AddressFamily field accordingly. Truncates IPv4
//     addresses to have a length of 4 bytes.
type sanitizationHook struct {
	maxNumWant          uint32
	defaultNumWant      uint32
	maxScrapeInfoHashes uint32
}

func (h *sanitizationHook) HandleAnnounce(ctx context.Context, req *bittorrent.AnnounceRequest, resp *bittorrent.AnnounceResponse) (context.Context, error) {
	if req.NumWant > h.maxNumWant {
		req.NumWant = h.maxNumWant
	}

	if req.NumWant == 0 {
		req.NumWant = h.defaultNumWant
	}

	if ip := req.Peer.IP.To4(); ip != nil {
		req.Peer.IP.IP = ip
		req.Peer.IP.AddressFamily = bittorrent.IPv4
	} else if len(req.Peer.IP.IP) == net.IPv6len { // implies req.Peer.IP.To4() == nil
		req.Peer.IP.AddressFamily = bittorrent.IPv6
	} else {
		return ctx, ErrInvalidIP
	}

	return ctx, nil
}

func (h *sanitizationHook) HandleScrape(ctx context.Context, req *bittorrent.ScrapeRequest, resp *bittorrent.ScrapeResponse) (context.Context, error) {
	if len(req.InfoHashes) > int(h.maxScrapeInfoHashes) {
		req.InfoHashes = req.InfoHashes[:h.maxScrapeInfoHashes]
	}

	return ctx, nil
}

func (h *sanitizationHook) HandleApi(ctx context.Context, req *bittorrent.ApiRequest, resp *bittorrent.ApiResponse) (context.Context, error) {
	// We trust ourselves, do we?
	return ctx, nil
}

type skipResponseHook struct{}

// SkipResponseHookKey is a key for the context of an Announce or Scrape to
// control whether the response middleware should run.
// Any non-nil value set for this key will cause the response middleware to
// skip.
var SkipResponseHookKey = skipResponseHook{}

type scrapeAddressType struct{}

// ScrapeIsIPv6Key is the key under which to store whether or not the
// address used to request a scrape was an IPv6 address.
// The value is expected to be of type bool.
// A missing value or a value that is not a bool for this key is equivalent to
// it being set to false.
var ScrapeIsIPv6Key = scrapeAddressType{}

type responseHook struct {
	store storage.PeerStore
}

func (h *responseHook) HandleAnnounce(ctx context.Context, req *bittorrent.AnnounceRequest, resp *bittorrent.AnnounceResponse) (_ context.Context, err error) {
	if ctx.Value(SkipResponseHookKey) != nil {
		return ctx, nil
	}

	// Add the Scrape data to the response.
	s := h.store.ScrapeSwarm(req.InfoHash, req.IP.AddressFamily)
	resp.Incomplete = s.Incomplete
	resp.Complete = s.Complete

	err = h.appendPeers(req, resp)
	return ctx, err
}

func (h *responseHook) appendPeers(req *bittorrent.AnnounceRequest, resp *bittorrent.AnnounceResponse) error {
	seeding := req.Left == 0
	peers, err := h.store.AnnouncePeers(req.InfoHash, seeding, int(req.NumWant), req.Peer)
	if err != nil && err != storage.ErrResourceDoesNotExist {
		return err
	}

	// Some clients expect a minimum of their own peer representation returned to
	// them if they are the only peer in a swarm.
	if len(peers) == 0 {
		peers = append(peers, req.Peer)
	}

	switch req.IP.AddressFamily {
	case bittorrent.IPv4:
		resp.IPv4Peers = peers
	case bittorrent.IPv6:
		resp.IPv6Peers = peers
	default:
		panic("attempted to append peer that is neither IPv4 nor IPv6")
	}

	return nil
}

func (h *responseHook) HandleScrape(ctx context.Context, req *bittorrent.ScrapeRequest, resp *bittorrent.ScrapeResponse) (context.Context, error) {
	if ctx.Value(SkipResponseHookKey) != nil {
		return ctx, nil
	}

	for _, infoHash := range req.InfoHashes {
		resp.Files = append(resp.Files, h.store.ScrapeSwarm(infoHash, req.AddressFamily))
	}

	return ctx, nil
}

func (h *responseHook) HandleApi(ctx context.Context, req *bittorrent.ApiRequest, resp *bittorrent.ApiResponse) (context.Context, error) {
	return ctx, nil
}
