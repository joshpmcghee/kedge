package router

import (
	"net/http"
	"net"
	"github.com/mwitkow/kedge/lib/resolvers"
	pb "github.com/mwitkow/kedge/_protogen/kedge/config/http/routes"
	"strings"
	"strconv"
	"fmt"
)

var (
	// DefaultALookup is the lookup resolver for DNS A records.
	// You can override it for caching or testing.
	DefaultALookup = net.LookupAddr
)


type AdhocAddresser interface {
	// Address decides the ip:port to send the request to, if any. Errors may be returned if permission is denied.
	// The returned string must contain contain both ip and port separated by colon.
	Address(r *http.Request) (string, error)
}


type addresser struct {
	rules []*pb.Adhoc
}



func NewAddresser(rules []*pb.Adhoc) AdhocAddresser {
	return &addresser{rules: rules}
}

func (a *addresser) Address(req *http.Request) (string, error) {
	hostName, port, err := a.extractHostPort(req.URL.Host)
	if err != nil {
		return "", err
	}
	for _, rule := range a.rules {
		if !a.hostMatches(req.URL.Host, rule.DnsNameMatcher) {
			continue
		}
		portForRule := port
		if port == 0 {
			if defPort := rule.Port.Default; defPort != 0 {
				portForRule = int(defPort)
			} else {
				portForRule = 80
			}
		}
		if !a.portAllowed(portForRule, rule.Port) {
			return "", NewError(http.StatusBadRequest, fmt.Sprintf("port %d is not allowed", portForRule))
		}
		if ipAddr, err := a.resolveHost(hostName); err != nil {
			return net.JoinHostPort(ipAddr, strconv.FormatInt(int64(portForRule), 10)), nil
		}
	}
	return "", ErrRouteNotFound
}

func (* addresser) resolveHost(hostStr string) (string, error) {
	addrs, err := DefaultALookup(hostStr)
	if err != nil {
		return "", NewError(http.StatusBadGateway, "cannot resolve host")
	}
	return addrs[0], nil
}

func (*addresser) extractHostPort(hostStr string) (hostName string , port int, err error) {
	// Using SplitHostPort is a pain due to opaque error messages. Let's assume we only do hostname matches, they fall
	// through later anyway.
	portOffset := strings.LastIndex(hostStr, ":")
	if portOffset == -1 {
		return hostStr, 0, nil
	}
	portPart := hostStr[portOffset:]
	pNum, err := strconv.ParseInt(portPart, 10, 16)
	if err != nil {
		return "", 0, NewError(http.StatusBadRequest, fmt.Sprintf("malformed port number: %v", err))
	}
	return hostStr[:portOffset], int(pNum), nil
}

func (*addresser) hostMatches(host string, matcher string) bool {
	if matcher == "" {
		return false
	}
	if matcher[0] != '*' {
		return host == matcher
	}
	return host == matcher[1:]
}

func (*addresser) portAllowed(port int, portRule *pb.Adhoc_Port) bool {
	uPort := uint32(port)
	for _, p := range portRule.Allowed {
		if p == uPort {
			return true
		}
	}
	for _, r := range portRule.AllowedRanges {
		if r.From <= uPort && uPort <= r.To {
			return true
		}
	}
	return false
}
