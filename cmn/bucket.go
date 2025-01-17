// Package cmn provides common constants, types, and utilities for AIS clients
// and AIStore.
/*
 * Copyright (c) 2018-2021, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
)

// Backend Provider enum
const (
	ProviderAIS    = "ais"
	ProviderAmazon = "aws"
	ProviderAzure  = "azure"
	ProviderGoogle = "gcp"
	ProviderHDFS   = "hdfs"
	ProviderHTTP   = "ht"
	allProviders   = "ais, aws (s3://), gcp (gs://), azure (az://), hdfs://, ht://"

	NsUUIDPrefix = '@' // BEWARE: used by on-disk layout
	NsNamePrefix = '#' // BEWARE: used by on-disk layout

	BckProviderSeparator = "://"

	// Scheme parsing
	DefaultScheme = "https"
	GSScheme      = "gs"
	S3Scheme      = "s3"
	AZScheme      = "az"
	AISScheme     = "ais"
)

type (
	// Ns (or Namespace) adds additional layer for scoping the data under
	// the same provider. It allows to have same dataset and bucket names
	// under different namespaces what allows for easy data manipulation without
	// affecting data in different namespaces.
	Ns struct {
		// UUID of other remote AIS cluster (for now only used for AIS). Note
		// that we can have different namespaces which refer to same UUID (cluster).
		// This means that in a sense UUID is a parent of the actual namespace.
		UUID string `json:"uuid" yaml:"uuid"`
		// Name uniquely identifies a namespace under the same UUID (which may
		// be empty) and is used in building FQN for the objects.
		Name string `json:"name" yaml:"name"`
	}

	Bck struct {
		Name     string       `json:"name" yaml:"name"`
		Provider string       `json:"provider" yaml:"provider"`
		Ns       Ns           `json:"namespace" yaml:"namespace" list:"omitempty"`
		Props    *BucketProps `json:"-"`
	}

	// Represents the AIS bucket, object and URL associated with a HTTP resource
	HTTPBckObj struct {
		Bck        Bck
		ObjName    string
		OrigURLBck string // HTTP URL of the bucket (object name excluded)
	}

	QueryBcks Bck

	Bcks []Bck

	// implemented by cluster.Bck
	NLP interface {
		Lock()
		TryLock(timeout time.Duration) bool
		TryRLock(timeout time.Duration) bool
		Unlock()
	}

	ParseURIOpts struct {
		DefaultProvider string // If set the provider will be used as provider.
		IsQuery         bool   // Determines if the URI should be parsed as query.
	}
)

var (
	// NsGlobal represents *this* cluster's global namespace that is used by default when
	// no specific namespace was defined or provided by the user.
	NsGlobal = Ns{}
	// NsAnyRemote represents any remote cluster. As such, NsGlobalRemote applies
	// exclusively to AIS (provider) given that other Backend providers are remote by definition.
	NsAnyRemote = Ns{UUID: string(NsUUIDPrefix)}

	Providers = cos.NewStringSet(
		ProviderAIS,
		ProviderGoogle,
		ProviderAmazon,
		ProviderAzure,
		ProviderHDFS,
		ProviderHTTP,
	)
)

////////////////
// Validation //
////////////////

// Validation for buckets is split into 2 cases:
//  1. Validation of concrete bucket, eg. get(bck, objName). In this case we
//     require the bucket name to be set. If the provider is not set the default
//     will be used, see `NormalizeProvider`.
//     This case is handled in `newBckFromQuery` and `newBckFromQueryUname`. The
//     CLI counterpart is `parseBckURI`.
//  2. Validation of query buckets, eg. list(queryBcks). Here all parts of the
//     bucket all optional.
//     This case is handled in `newQueryBcksFromQuery`. The CLI counterpart is
//     `parseQueryBckURI`.
// These 2 cases have a slightly different logic for the validation but the
// validation functions are always the same. Bucket name (`bck.ValidateName`)
// and bucket namespace (`bck.Ns.Validate`) validation is quite straightforward
// as we only need to check if the strings contain only valid characters. Bucket
// provider validation on the other hand a little bit more tricky as we have so
// called "normalized providers" and their aliases. Normalized providers are the
// providers registered in `Providers` set. Almost any provider that is being
// validated goes through `NormalizeProvider` which converts aliases to
// normalized form or sets default provider if the provider is empty. But there
// are cases where we already expect **only** the normalized providers, for
// example in FQN parsing. For this case `IsNormalizedProvider` function must be
// used.
//
// Similar concepts are applied when bucket is provided as URI,
// eg. `ais://@uuid#ns/bucket_name`. URI form is heavily used by CLI. Parsing
// is handled by `ParseBckObjectURI` which by itself doesn't do much validation.
// The validation happens in aforementioned CLI specific parse functions.

// IsNormalizedProvider returns true if the provider is in normalized
// form (`aws`, `gcp`, etc.), not aliased (`s3`, `gs`, etc.). Only providers
// registered in `Providers` set are considered normalized.
func IsNormalizedProvider(provider string) bool {
	_, exists := Providers[provider]
	return exists
}

// NormalizeProvider replaces provider aliases with their normalized form/name.
func NormalizeProvider(provider string) (string, error) {
	switch provider {
	case "":
		// NOTE: Here is place to change default provider.
		return ProviderAIS, nil
	case S3Scheme:
		return ProviderAmazon, nil
	case AZScheme:
		return ProviderAzure, nil
	case GSScheme:
		return ProviderGoogle, nil
	default:
		if !IsNormalizedProvider(provider) {
			return provider, NewErrorInvalidBucketProvider(Bck{Provider: provider})
		}
		return provider, nil
	}
}

// Parses "[provider://][@uuid#namespace][/][bucketName[/objectName]]"
func ParseBckObjectURI(uri string, opts ParseURIOpts) (bck Bck, objName string, err error) {
	debug.Assert(opts.DefaultProvider == "" || IsNormalizedProvider(opts.DefaultProvider))

	const bucketSepa = "/"
	parts := strings.SplitN(uri, BckProviderSeparator, 2)
	if len(parts) > 1 && parts[0] != "" {
		bck.Provider, err = NormalizeProvider(parts[0])
		uri = parts[1]
	} else if !opts.IsQuery {
		bck.Provider = opts.DefaultProvider
	}

	if err != nil {
		return
	}

	parts = strings.SplitN(uri, bucketSepa, 2)
	if len(parts[0]) > 0 && (parts[0][0] == NsUUIDPrefix || parts[0][0] == NsNamePrefix) {
		bck.Ns = ParseNsUname(parts[0])
		if err := bck.Ns.Validate(); err != nil {
			return bck, "", err
		}
		if !opts.IsQuery && bck.Provider == "" {
			return bck, "",
				fmt.Errorf("provider cannot be empty when namespace is not (did you mean \"ais://%s\"?)", bck.String())
		}
		if len(parts) == 1 {
			if parts[0] == string(NsUUIDPrefix) && opts.IsQuery {
				// Case: "[provider://]@" (only valid if uri is query)
				// We need to list buckets from all possible remote clusters
				bck.Ns = NsAnyRemote
				return bck, "", nil
			}

			// Case: "[provider://]@uuid#ns"
			return bck, "", nil
		}

		// Case: "[provider://]@uuid#ns/bucket"
		parts = strings.SplitN(parts[1], bucketSepa, 2)
	}

	bck.Name = parts[0]
	if bck.Name != "" {
		if err := bck.ValidateName(); err != nil {
			return bck, "", err
		}
		if bck.Provider == "" {
			return bck, "", fmt.Errorf("provider cannot be empty - did you mean: \"ais://%s\"?", bck.String())
		}
	}
	if len(parts) > 1 {
		objName = parts[1]
	}
	return
}

////////
// Ns //
////////

// Parses [@uuid][#namespace]. It does a little bit more than just parsing
// a string from `Uname` so that logic can be reused in different places.
func ParseNsUname(s string) (n Ns) {
	if len(s) > 0 && s[0] == NsUUIDPrefix {
		s = s[1:]
	}
	idx := strings.IndexByte(s, NsNamePrefix)
	if idx == -1 {
		n.UUID = s
	} else {
		n.UUID = s[:idx]
		n.Name = s[idx+1:]
	}
	return
}

func (n Ns) String() string {
	if n.IsGlobal() {
		return ""
	}
	res := ""
	if n.UUID != "" {
		res += string(NsUUIDPrefix) + n.UUID
	}
	if n.Name != "" {
		res += string(NsNamePrefix) + n.Name
	}
	return res
}

func (n Ns) Uname() string {
	b := make([]byte, 0, 2+len(n.UUID)+len(n.Name))
	b = append(b, NsUUIDPrefix)
	b = append(b, n.UUID...)
	b = append(b, NsNamePrefix)
	b = append(b, n.Name...)
	return string(b)
}

func (n Ns) Validate() error {
	if cos.IsAlphaPlus(n.UUID, false /*with period*/) && cos.IsAlphaPlus(n.Name, false) {
		return nil
	}
	return fmt.Errorf(
		"namespace (uuid: %q, name: %q) may only contain letters, numbers, dashes (-), underscores (_)",
		n.UUID, n.Name,
	)
}

func (n Ns) Contains(other Ns) bool {
	if n.IsGlobal() {
		return true // If query is empty (global) we accept any namespace
	}
	if n.IsAnyRemote() {
		return other.IsRemote()
	}
	return n == other
}

/////////
// Bck //
/////////

func (b Bck) Less(other Bck) bool {
	if QueryBcks(b).Contains(other) {
		return true
	}
	if b.Provider != other.Provider {
		return b.Provider < other.Provider
	}
	sb, so := b.Ns.String(), other.Ns.String()
	if sb != so {
		return sb < so
	}
	return b.Name < other.Name
}

func (b Bck) Equal(other Bck) bool {
	return b.Name == other.Name && b.Provider == other.Provider && b.Ns == other.Ns
}

func (b *Bck) Validate() (err error) {
	if err := b.ValidateName(); err != nil {
		return err
	}
	b.Provider, err = NormalizeProvider(b.Provider)
	if err != nil {
		return err
	}
	return b.Ns.Validate()
}

func (b *Bck) ValidateName() (err error) {
	if b.Name == "" || b.Name == "." {
		return fmt.Errorf(fmtErrBckName, b.Name)
	}
	if !cos.IsAlphaPlus(b.Name, true /*with period*/) {
		err = fmt.Errorf(fmtErrBckName, b.Name)
	}
	return
}

func (b Bck) String() string {
	if b.Ns.IsGlobal() {
		if b.Provider == "" {
			return b.Name
		}
		return fmt.Sprintf("%s%s%s", b.Provider, BckProviderSeparator, b.Name)
	}
	if b.Provider == "" {
		return fmt.Sprintf("%s/%s", b.Ns, b.Name)
	}
	return fmt.Sprintf("%s%s%s/%s", b.Provider, BckProviderSeparator, b.Ns, b.Name)
}

func (b Bck) IsEmpty() bool { return b.Name == "" && b.Provider == "" && b.Ns == NsGlobal }

// Bck => unique name (use ParseUname below to translate back)
func (b Bck) MakeUname(objName string) string {
	var (
		nsUname = b.Ns.Uname()
		l       = len(b.Provider) + 1 + len(nsUname) + 1 + len(b.Name) + 1 + len(objName)
		buf     = make([]byte, 0, l)
	)
	buf = append(buf, b.Provider...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, nsUname...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, b.Name...)
	buf = append(buf, filepath.Separator)
	buf = append(buf, objName...)
	return *(*string)(unsafe.Pointer(&buf))
}

// unique name => Bck (use MakeUname above to perform the reverse translation)
func ParseUname(uname string) (b Bck, objName string) {
	var prev, itemIdx int
	for i := 0; i < len(uname); i++ {
		if uname[i] != filepath.Separator {
			continue
		}

		item := uname[prev:i]
		switch itemIdx {
		case 0:
			b.Provider = item
		case 1:
			b.Ns = ParseNsUname(item)
		case 2:
			b.Name = item
			objName = uname[i+1:]
			return
		}

		itemIdx++
		prev = i + 1
	}
	return
}

//
// Is-Whats
//

func IsCloudProvider(p string) bool {
	return p == ProviderAmazon || p == ProviderGoogle || p == ProviderAzure
}

func (n Ns) IsGlobal() bool    { return n == NsGlobal }
func (n Ns) IsAnyRemote() bool { return n == NsAnyRemote }
func (n Ns) IsRemote() bool    { return n.UUID != "" }

func (b *Bck) HasBackendBck() bool {
	return b.Provider == ProviderAIS && b.Props != nil && !b.Props.BackendBck.IsEmpty()
}

func (b *Bck) BackendBck() *Bck {
	if b.HasBackendBck() {
		return &b.Props.BackendBck
	}
	return nil
}

func (b *Bck) RemoteBck() *Bck {
	if !b.IsRemote() {
		return nil
	}
	if b.HasBackendBck() {
		return &b.Props.BackendBck
	}
	return b
}

func (b Bck) IsAIS() bool       { return b.Provider == ProviderAIS && !b.Ns.IsRemote() && !b.HasBackendBck() }
func (b Bck) IsRemoteAIS() bool { return b.Provider == ProviderAIS && b.Ns.IsRemote() }
func (b Bck) IsHDFS() bool      { return b.Provider == ProviderHDFS }
func (b Bck) IsHTTP() bool      { return b.Provider == ProviderHTTP }

func (b Bck) IsRemote() bool {
	return b.IsCloud() || b.IsRemoteAIS() || b.IsHDFS() || b.IsHTTP() || b.HasBackendBck()
}

func (b Bck) IsCloud() bool {
	if bck := b.BackendBck(); bck != nil {
		debug.Assert(bck.IsCloud()) // Currently, backend bucket is always cloud.
		return bck.IsCloud()
	}
	return IsCloudProvider(b.Provider)
}

func (b Bck) HasProvider() bool {
	if b.Provider != "" {
		// If the provider is set it must be valid.
		debug.Assert(IsNormalizedProvider(b.Provider))
		return true
	}
	return false
}

func (query QueryBcks) String() string    { return Bck(query).String() }
func (query QueryBcks) IsAIS() bool       { return Bck(query).IsAIS() }
func (query QueryBcks) IsHDFS() bool      { return Bck(query).IsHDFS() }
func (query QueryBcks) IsRemoteAIS() bool { return Bck(query).IsRemoteAIS() }
func (query QueryBcks) IsCloud() bool     { return IsCloudProvider(query.Provider) }

func (query *QueryBcks) Validate() (err error) {
	if query.Name != "" {
		bck := Bck(*query)
		if err := bck.ValidateName(); err != nil {
			return err
		}
	}
	if query.Provider != "" {
		query.Provider, err = NormalizeProvider(query.Provider)
		if err != nil {
			return err
		}
	}
	if query.Ns != NsGlobal && query.Ns != NsAnyRemote {
		return query.Ns.Validate()
	}
	return nil
}
func (query QueryBcks) Equal(bck Bck) bool { return Bck(query).Equal(bck) }
func (query QueryBcks) Contains(other Bck) bool {
	if query.Name != "" {
		// NOTE: named bucket with no provider is assumed to be ais://
		if other.Provider == "" {
			other.Provider = ProviderAIS
		}
		if query.Provider == "" {
			// If query's provider not set, we should match the expected bucket
			query.Provider = other.Provider // nolint:revive // temp change to compare
		}
		return query.Equal(other)
	}
	ok := query.Provider == other.Provider || query.Provider == ""
	return ok && query.Ns.Contains(other.Ns)
}

func AddBckToQuery(query url.Values, bck Bck) url.Values {
	if bck.Provider != "" {
		if query == nil {
			query = make(url.Values)
		}
		query.Set(URLParamProvider, bck.Provider)
	}
	if !bck.Ns.IsGlobal() {
		if query == nil {
			query = make(url.Values)
		}
		query.Set(URLParamNamespace, bck.Ns.Uname())
	}
	return query
}

func AddBckUnameToQuery(query url.Values, bck Bck, uparam string) url.Values {
	if query == nil {
		query = make(url.Values)
	}
	uname := bck.MakeUname("")
	query.Set(uparam, uname)
	return query
}

func DelBckFromQuery(query url.Values) url.Values {
	query.Del(URLParamProvider)
	query.Del(URLParamNamespace)
	return query
}

//////////
// Bcks //
//////////

func (bcks Bcks) Len() int {
	return len(bcks)
}

func (bcks Bcks) Less(i, j int) bool {
	return bcks[i].Less(bcks[j])
}

func (bcks Bcks) Swap(i, j int) {
	bcks[i], bcks[j] = bcks[j], bcks[i]
}

func (bcks Bcks) Select(query QueryBcks) (filtered Bcks) {
	for _, bck := range bcks {
		if query.Contains(bck) {
			filtered = append(filtered, bck)
		}
	}
	return filtered
}

func (bcks Bcks) Contains(query QueryBcks) bool {
	for _, bck := range bcks {
		if query.Equal(bck) || query.Contains(bck) {
			return true
		}
	}
	return false
}

func (bcks Bcks) Equal(other Bcks) bool {
	if len(bcks) != len(other) {
		return false
	}
	for _, b1 := range bcks {
		var found bool
		for _, b2 := range other {
			if b1.Equal(b2) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

////////////////
// HTTPBckObj //
////////////////

func NewHTTPObj(u *url.URL) *HTTPBckObj {
	hbo := &HTTPBckObj{
		Bck: Bck{
			Provider: ProviderHTTP,
			Ns:       NsGlobal,
		},
	}
	hbo.OrigURLBck, hbo.ObjName = filepath.Split(u.Path)
	hbo.OrigURLBck = u.Scheme + "://" + u.Host + hbo.OrigURLBck
	hbo.Bck.Name = cos.OrigURLBck2Name(hbo.OrigURLBck)
	return hbo
}

func NewHTTPObjPath(rawURL string) (*HTTPBckObj, error) {
	urlObj, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, err
	}
	return NewHTTPObj(urlObj), nil
}
