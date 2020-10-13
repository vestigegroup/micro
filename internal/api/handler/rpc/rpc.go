// Copyright 2020 Asim Aslam
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Original source: github.com/micro/go-micro/v3/api/handler/rpc/rpc.go

// Package rpc is a go-micro rpc handler.
package rpc

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/micro/go-micro/v3/client"
	"github.com/micro/go-micro/v3/codec"
	"github.com/micro/go-micro/v3/codec/bytes"
	"github.com/micro/go-micro/v3/codec/jsonrpc"
	"github.com/micro/go-micro/v3/codec/protorpc"
	"github.com/micro/go-micro/v3/errors"
	"github.com/micro/go-micro/v3/metadata"
	"github.com/micro/go-micro/v3/util/ctx"
	"github.com/micro/go-micro/v3/util/qson"
	"github.com/micro/go-micro/v3/util/router"
	"github.com/micro/micro/v3/internal/api/handler"
	"github.com/micro/micro/v3/service/api"
	"github.com/micro/micro/v3/service/logger"
	"github.com/oxtoacart/bpool"
)

const (
	Handler = "rpc"
)

var (
	// supported json codecs
	jsonCodecs = []string{
		"application/grpc+json",
		"application/json",
		"application/json-rpc",
	}

	// support proto codecs
	protoCodecs = []string{
		"application/grpc",
		"application/grpc+proto",
		"application/proto",
		"application/protobuf",
		"application/proto-rpc",
		"application/octet-stream",
	}

	bufferPool = bpool.NewSizedBufferPool(1024, 8)
)

type rpcHandler struct {
	opts handler.Options
	s    *api.Service
}

type buffer struct {
	io.ReadCloser
}

func (b *buffer) Write(_ []byte) (int, error) {
	return 0, nil
}

func (h *rpcHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bsize := handler.DefaultMaxRecvSize
	if h.opts.MaxRecvSize > 0 {
		bsize = h.opts.MaxRecvSize
	}

	r.Body = http.MaxBytesReader(w, r.Body, bsize)

	defer r.Body.Close()
	var service *api.Service

	if h.s != nil {
		// we were given the service
		service = h.s
	} else if h.opts.Router != nil {
		// try get service from router
		s, err := h.opts.Router.Route(r)
		if err != nil {
			writeError(w, r, errors.InternalServerError("go.micro.api", err.Error()))
			return
		}
		service = s
	} else {
		// we have no way of routing the request
		writeError(w, r, errors.InternalServerError("go.micro.api", "no route found"))
		return
	}

	ct := r.Header.Get("Content-Type")

	// Strip charset from Content-Type (like `application/json; charset=UTF-8`)
	if idx := strings.IndexRune(ct, ';'); idx >= 0 {
		ct = ct[:idx]
	}

	// micro client
	c := h.opts.Client

	// create context
	cx := ctx.FromRequest(r)

	// set merged context to request
	*r = *r.Clone(cx)
	// if stream we currently only support json
	if isStream(r, service) {
		serveWebsocket(cx, w, r, service, c)
		return
	}

	// create custom router
	callOpt := client.WithRouter(router.New(service.Services))

	// walk the standard call path
	// get payload
	br, err := requestPayload(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var rsp []byte

	switch {
	// proto codecs
	case hasCodec(ct, protoCodecs):
		var request *bytes.Frame
		// if the extracted payload isn't empty lets use it
		if len(br) > 0 {
			request = &bytes.Frame{Data: br}
		}

		// create the request
		req := c.NewRequest(
			service.Name,
			service.Endpoint.Name,
			request,
			client.WithContentType(ct),
		)

		// make the call
		var response *bytes.Frame
		if err := c.Call(cx, req, response, callOpt); err != nil {
			writeError(w, r, err)
			return
		}
		rsp = response.Data
	default:
		// if json codec is not present set to json
		if !hasCodec(ct, jsonCodecs) {
			ct = "application/json"
		}

		// default to trying json
		var request json.RawMessage
		// if the extracted payload isn't empty lets use it
		if len(br) > 0 {
			request = json.RawMessage(br)
		}

		// create request/response
		var response json.RawMessage

		req := c.NewRequest(
			service.Name,
			service.Endpoint.Name,
			&request,
			client.WithContentType(ct),
		)
		// make the call
		if err := c.Call(cx, req, &response, callOpt); err != nil {
			writeError(w, r, err)
			return
		}

		// marshall response
		rsp, err = response.MarshalJSON()
		if err != nil {
			writeError(w, r, err)
			return
		}
	}

	// write the response
	writeResponse(w, r, rsp)
}

func (rh *rpcHandler) String() string {
	return "rpc"
}

func hasCodec(ct string, codecs []string) bool {
	for _, codec := range codecs {
		if ct == codec {
			return true
		}
	}
	return false
}

// requestPayload takes a *http.Request.
// If the request is a GET the query string parameters are extracted and marshaled to JSON and the raw bytes are returned.
// If the request method is a POST the request body is read and returned
func requestPayload(r *http.Request) ([]byte, error) {
	var err error

	// we have to decode json-rpc and proto-rpc because we suck
	// well actually because there's no proxy codec right now

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "application/json-rpc"):
		msg := codec.Message{
			Type:   codec.Request,
			Header: make(map[string]string),
		}
		c := jsonrpc.NewCodec(&buffer{r.Body})
		if err = c.ReadHeader(&msg, codec.Request); err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err = c.ReadBody(&raw); err != nil {
			return nil, err
		}
		return ([]byte)(raw), nil
	case strings.Contains(ct, "application/proto-rpc"), strings.Contains(ct, "application/octet-stream"):
		msg := codec.Message{
			Type:   codec.Request,
			Header: make(map[string]string),
		}
		c := protorpc.NewCodec(&buffer{r.Body})
		if err = c.ReadHeader(&msg, codec.Request); err != nil {
			return nil, err
		}
		var raw *bytes.Frame
		if err = c.ReadBody(&raw); err != nil {
			return nil, err
		}
		return raw.Data, nil
	case strings.Contains(ct, "application/x-www-form-urlencoded"):
		r.ParseForm()

		// generate a new set of values from the form
		vals := make(map[string]string)
		for k, v := range r.Form {
			vals[k] = strings.Join(v, ",")
		}

		// marshal
		return json.Marshal(vals)
		// TODO: application/grpc
	}

	// otherwise as per usual
	ctx := r.Context()
	// dont user metadata.FromContext as it mangles names
	md, ok := metadata.FromContext(ctx)
	if !ok {
		md = make(map[string]string)
	}

	// allocate maximum
	matches := make(map[string]interface{}, len(md))
	bodydst := ""

	// get fields from url path
	for k, v := range md {
		k = strings.ToLower(k)
		// filter own keys
		if strings.HasPrefix(k, "x-api-field-") {
			matches[strings.TrimPrefix(k, "x-api-field-")] = v
			delete(md, k)
		} else if k == "x-api-body" {
			bodydst = v
			delete(md, k)
		}
	}

	// map of all fields
	req := make(map[string]interface{}, len(md))

	// get fields from url values
	if len(r.URL.RawQuery) > 0 {
		umd := make(map[string]interface{})
		err = qson.Unmarshal(&umd, r.URL.RawQuery)
		if err != nil {
			return nil, err
		}
		for k, v := range umd {
			matches[k] = v
		}
	}

	// restore context without fields
	*r = *r.Clone(metadata.NewContext(ctx, md))

	for k, v := range matches {
		ps := strings.Split(k, ".")
		if len(ps) == 1 {
			req[k] = v
			continue
		}
		em := make(map[string]interface{})
		em[ps[len(ps)-1]] = v
		for i := len(ps) - 2; i > 0; i-- {
			nm := make(map[string]interface{})
			nm[ps[i]] = em
			em = nm
		}
		if vm, ok := req[ps[0]]; ok {
			// nested map
			nm := vm.(map[string]interface{})
			for vk, vv := range em {
				nm[vk] = vv
			}
			req[ps[0]] = nm
		} else {
			req[ps[0]] = em
		}
	}
	pathbuf := []byte("{}")
	if len(req) > 0 {
		pathbuf, err = json.Marshal(req)
		if err != nil {
			return nil, err
		}
	}

	urlbuf := []byte("{}")
	out, err := jsonpatch.MergeMergePatches(urlbuf, pathbuf)
	if err != nil {
		return nil, err
	}

	switch r.Method {
	case "GET":
		// empty response
		if strings.Contains(ct, "application/json") && string(out) == "{}" {
			return out, nil
		} else if string(out) == "{}" && !strings.Contains(ct, "application/json") {
			return []byte{}, nil
		}
		return out, nil
	case "PATCH", "POST", "PUT", "DELETE":
		bodybuf := []byte("{}")
		buf := bufferPool.Get()
		defer bufferPool.Put(buf)
		if _, err := buf.ReadFrom(r.Body); err != nil {
			return nil, err
		}
		if b := buf.Bytes(); len(b) > 0 {
			bodybuf = b
		}
		if bodydst == "" || bodydst == "*" {
			// jsonpatch resequences the json object so we avoid it if possible (some usecases such as
			// validating signatures require the request body to be unchangedd). We're keeping support
			// for the custom paramaters for backwards compatability reasons.
			if string(out) == "{}" {
				return bodybuf, nil
			}

			if out, err = jsonpatch.MergeMergePatches(out, bodybuf); err == nil {
				return out, nil
			}
		}
		var jsonbody map[string]interface{}
		if json.Valid(bodybuf) {
			if err = json.Unmarshal(bodybuf, &jsonbody); err != nil {
				return nil, err
			}
		}
		dstmap := make(map[string]interface{})
		ps := strings.Split(bodydst, ".")
		if len(ps) == 1 {
			if jsonbody != nil {
				dstmap[ps[0]] = jsonbody
			} else {
				// old unexpected behaviour
				dstmap[ps[0]] = bodybuf
			}
		} else {
			em := make(map[string]interface{})
			if jsonbody != nil {
				em[ps[len(ps)-1]] = jsonbody
			} else {
				// old unexpected behaviour
				em[ps[len(ps)-1]] = bodybuf
			}
			for i := len(ps) - 2; i > 0; i-- {
				nm := make(map[string]interface{})
				nm[ps[i]] = em
				em = nm
			}
			dstmap[ps[0]] = em
		}

		bodyout, err := json.Marshal(dstmap)
		if err != nil {
			return nil, err
		}

		if out, err = jsonpatch.MergeMergePatches(out, bodyout); err == nil {
			return out, nil
		}

		//fallback to previous unknown behaviour
		return bodybuf, nil
	}

	return []byte{}, nil
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	ce := errors.Parse(err.Error())

	switch ce.Code {
	case 0:
		// assuming it's totally screwed
		ce.Code = 500
		ce.Id = "go.micro.api"
		ce.Status = http.StatusText(500)
		ce.Detail = "error during request: " + ce.Detail
		w.WriteHeader(500)
	default:
		w.WriteHeader(int(ce.Code))
	}

	// response content type
	w.Header().Set("Content-Type", "application/json")

	// Set trailers
	if strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
		w.Header().Set("Trailer", "grpc-status")
		w.Header().Set("Trailer", "grpc-message")
		w.Header().Set("grpc-status", "13")
		w.Header().Set("grpc-message", ce.Detail)
	}

	_, werr := w.Write([]byte(ce.Error()))
	if werr != nil {
		if logger.V(logger.ErrorLevel, logger.DefaultLogger) {
			logger.Error(werr)
		}
	}
}

func writeResponse(w http.ResponseWriter, r *http.Request, rsp []byte) {
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	w.Header().Set("Content-Length", strconv.Itoa(len(rsp)))

	// Set trailers
	if strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
		w.Header().Set("Trailer", "grpc-status")
		w.Header().Set("Trailer", "grpc-message")
		w.Header().Set("grpc-status", "0")
		w.Header().Set("grpc-message", "")
	}

	// write 204 status if rsp is nil
	if len(rsp) == 0 {
		w.WriteHeader(http.StatusNoContent)
	}

	// write response
	_, err := w.Write(rsp)
	if err != nil {
		if logger.V(logger.ErrorLevel, logger.DefaultLogger) {
			logger.Error(err)
		}
	}

}

func NewHandler(opts ...handler.Option) handler.Handler {
	options := handler.NewOptions(opts...)
	return &rpcHandler{
		opts: options,
	}
}

func WithService(s *api.Service, opts ...handler.Option) handler.Handler {
	options := handler.NewOptions(opts...)
	return &rpcHandler{
		opts: options,
		s:    s,
	}
}