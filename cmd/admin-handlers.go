/*
 * Minio Cloud Storage, (C) 2016, 2017, 2018 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/scriptburn/minio/cmd/logger"
	"github.com/scriptburn/minio/pkg/auth"
	"github.com/scriptburn/minio/pkg/cpu"
	"github.com/scriptburn/minio/pkg/disk"
	"github.com/scriptburn/minio/pkg/handlers"
	"github.com/scriptburn/minio/pkg/iam/policy"
	"github.com/scriptburn/minio/pkg/madmin"
	"github.com/scriptburn/minio/pkg/mem"
	xnet "github.com/scriptburn/minio/pkg/net"
	"github.com/scriptburn/minio/pkg/quick"
)

const (
	maxEConfigJSONSize = 262272
)

// Type-safe query params.
type mgmtQueryKey string

// Only valid query params for mgmt admin APIs.
const (
	mgmtBucket      mgmtQueryKey = "bucket"
	mgmtPrefix                   = "prefix"
	mgmtClientToken              = "clientToken"
	mgmtForceStart               = "forceStart"
	mgmtForceStop                = "forceStop"
)

var (
	// This struct literal represents the Admin API version that
	// the server uses.
	adminAPIVersionInfo = madmin.AdminAPIVersionInfo{
		Version: "1",
	}
)

// VersionHandler - GET /minio/admin/version
// -----------
// Returns Administration API version
func (a adminAPIHandlers) VersionHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "Version")

	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	jsonBytes, err := json.Marshal(adminAPIVersionInfo)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, jsonBytes)
}

// ServiceStatusHandler - GET /minio/admin/v1/service
// ----------
// Returns server version and uptime.
func (a adminAPIHandlers) ServiceStatusHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ServiceStatus")

	if globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Fetch server version
	serverVersion := madmin.ServerVersion{
		Version:  Version,
		CommitID: CommitID,
	}

	// Fetch uptimes from all peers and pick the latest.
	uptime := getPeerUptimes(globalNotificationSys.ServerInfo(ctx))

	// Create API response
	serverStatus := madmin.ServiceStatus{
		ServerVersion: serverVersion,
		Uptime:        uptime,
	}

	// Marshal API response
	jsonBytes, err := json.Marshal(serverStatus)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Reply with storage information (across nodes in a
	// distributed setup) as json.
	writeSuccessResponseJSON(w, jsonBytes)
}

// ServiceStopNRestartHandler - POST /minio/admin/v1/service
// Body: {"action": <restart-action>}
// ----------
// Restarts/Stops minio server gracefully. In a distributed setup,
// restarts all the servers in the cluster.
func (a adminAPIHandlers) ServiceStopNRestartHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ServiceStopNRestart")

	if globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	var sa madmin.ServiceAction
	err := json.NewDecoder(r.Body).Decode(&sa)
	if err != nil {
		writeErrorResponseJSON(w, ErrRequestBodyParse, r.URL)
		return
	}

	var serviceSig serviceSignal
	switch sa.Action {
	case madmin.ServiceActionValueRestart:
		serviceSig = serviceRestart
	case madmin.ServiceActionValueStop:
		serviceSig = serviceStop
	default:
		writeErrorResponseJSON(w, ErrMalformedPOSTRequest, r.URL)
		logger.LogIf(ctx, errors.New("Invalid service action received"))
		return
	}

	// Reply to the client before restarting minio server.
	writeSuccessResponseHeadersOnly(w)

	// Notify all other Minio peers signal service.
	for _, nerr := range globalNotificationSys.SignalService(serviceSig) {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}

	globalServiceSignalCh <- serviceSig
}

// ServerProperties holds some server information such as, version, region
// uptime, etc..
type ServerProperties struct {
	Uptime   time.Duration `json:"uptime"`
	Version  string        `json:"version"`
	CommitID string        `json:"commitID"`
	Region   string        `json:"region"`
	SQSARN   []string      `json:"sqsARN"`
}

// ServerConnStats holds transferred bytes from/to the server
type ServerConnStats struct {
	TotalInputBytes  uint64 `json:"transferred"`
	TotalOutputBytes uint64 `json:"received"`
	Throughput       uint64 `json:"throughput,omitempty"`
}

// ServerHTTPMethodStats holds total number of HTTP operations from/to the server,
// including the average duration the call was spent.
type ServerHTTPMethodStats struct {
	Count       uint64 `json:"count"`
	AvgDuration string `json:"avgDuration"`
}

// ServerHTTPStats holds all type of http operations performed to/from the server
// including their average execution time.
type ServerHTTPStats struct {
	TotalHEADStats     ServerHTTPMethodStats `json:"totalHEADs"`
	SuccessHEADStats   ServerHTTPMethodStats `json:"successHEADs"`
	TotalGETStats      ServerHTTPMethodStats `json:"totalGETs"`
	SuccessGETStats    ServerHTTPMethodStats `json:"successGETs"`
	TotalPUTStats      ServerHTTPMethodStats `json:"totalPUTs"`
	SuccessPUTStats    ServerHTTPMethodStats `json:"successPUTs"`
	TotalPOSTStats     ServerHTTPMethodStats `json:"totalPOSTs"`
	SuccessPOSTStats   ServerHTTPMethodStats `json:"successPOSTs"`
	TotalDELETEStats   ServerHTTPMethodStats `json:"totalDELETEs"`
	SuccessDELETEStats ServerHTTPMethodStats `json:"successDELETEs"`
}

// ServerInfoData holds storage, connections and other
// information of a given server.
type ServerInfoData struct {
	StorageInfo StorageInfo      `json:"storage"`
	ConnStats   ServerConnStats  `json:"network"`
	HTTPStats   ServerHTTPStats  `json:"http"`
	Properties  ServerProperties `json:"server"`
}

// ServerInfo holds server information result of one node
type ServerInfo struct {
	Error string          `json:"error"`
	Addr  string          `json:"addr"`
	Data  *ServerInfoData `json:"data"`
}

// ServerInfoHandler - GET /minio/admin/v1/info
// ----------
// Get server information
func (a adminAPIHandlers) ServerInfoHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ServerInfo")

	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Authenticate request

	// Setting the region as empty so as the mc server info command is irrespective to the region.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	thisAddr, err := xnet.ParseHost(GetLocalPeer(globalEndpoints))
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	serverInfo := globalNotificationSys.ServerInfo(ctx)
	// Once we have received all the ServerInfo from peers
	// add the local peer server info as well.
	serverInfo = append(serverInfo, ServerInfo{
		Addr: thisAddr.String(),
		Data: &ServerInfoData{
			StorageInfo: objectAPI.StorageInfo(ctx),
			ConnStats:   globalConnStats.toServerConnStats(),
			HTTPStats:   globalHTTPStats.toServerHTTPStats(),
			Properties: ServerProperties{
				Uptime:   UTCNow().Sub(globalBootTime),
				Version:  Version,
				CommitID: CommitID,
				SQSARN:   globalNotificationSys.GetARNList(),
				Region:   globalServerConfig.GetRegion(),
			},
		},
	})

	// Marshal API response
	jsonBytes, err := json.Marshal(serverInfo)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Reply with storage information (across nodes in a
	// distributed setup) as json.
	writeSuccessResponseJSON(w, jsonBytes)
}

// ServerDrivesPerfInfo holds informantion about address, performance
// of all drives on one server. It also reports any errors if encountered
// while trying to reach this server.
type ServerDrivesPerfInfo struct {
	Addr  string             `json:"addr"`
	Error string             `json:"error,omitempty"`
	Perf  []disk.Performance `json:"perf"`
}

// ServerCPULoadInfo holds informantion about cpu utilization
// of one minio node. It also reports any errors if encountered
// while trying to reach this server.
type ServerCPULoadInfo struct {
	Addr  string     `json:"addr"`
	Error string     `json:"error,omitempty"`
	Load  []cpu.Load `json:"load"`
}

// ServerMemUsageInfo holds informantion about memory utilization
// of one minio node. It also reports any errors if encountered
// while trying to reach this server.
type ServerMemUsageInfo struct {
	Addr  string      `json:"addr"`
	Error string      `json:"error,omitempty"`
	Usage []mem.Usage `json:"usage"`
}

// PerfInfoHandler - GET /minio/admin/v1/performance?perfType={perfType}
// ----------
// Get all performance information based on input type
// Supported types = drive
func (a adminAPIHandlers) PerfInfoHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "PerfInfo")

	// Get object layer instance.
	objLayer := newObjectLayerFn()
	if objLayer == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Authenticate request
	// Setting the region as empty so as the mc server info command is irrespective to the region.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	vars := mux.Vars(r)
	perfType := vars["perfType"]

	if perfType == "drive" {
		info := objLayer.StorageInfo(ctx)
		if !(info.Backend.Type == BackendFS || info.Backend.Type == BackendErasure) {

			writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
			return
		}
		// Get drive performance details from local server's drive(s)
		dp := localEndpointsDrivePerf(globalEndpoints)

		// Notify all other Minio peers to report drive performance numbers
		dps := globalNotificationSys.DrivePerfInfo()
		dps = append(dps, dp)

		// Marshal API response
		jsonBytes, err := json.Marshal(dps)
		if err != nil {
			writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
			return
		}

		// Reply with performance information (across nodes in a
		// distributed setup) as json.
		writeSuccessResponseJSON(w, jsonBytes)
	} else if perfType == "cpu" {
		// Get CPU load details from local server's cpu(s)
		cpu := localEndpointsCPULoad(globalEndpoints)
		// Notify all other Minio peers to report cpu load numbers
		cpus := globalNotificationSys.CPULoadInfo()
		cpus = append(cpus, cpu)

		// Marshal API response
		jsonBytes, err := json.Marshal(cpus)
		if err != nil {
			writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
			return
		}

		// Reply with cpu load information (across nodes in a
		// distributed setup) as json.
		writeSuccessResponseJSON(w, jsonBytes)
	} else if perfType == "mem" {
		// Get mem usage details from local server(s)
		m := localEndpointsMemUsage(globalEndpoints)
		// Notify all other Minio peers to report mem usage numbers
		mems := globalNotificationSys.MemUsageInfo()
		mems = append(mems, m)

		// Marshal API response
		jsonBytes, err := json.Marshal(mems)
		if err != nil {
			writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
			return
		}

		// Reply with mem usage information (across nodes in a
		// distributed setup) as json.
		writeSuccessResponseJSON(w, jsonBytes)
	} else {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
	}
	return
}

// StartProfilingResult contains the status of the starting
// profiling action in a given server
type StartProfilingResult struct {
	NodeName string `json:"nodeName"`
	Success  bool   `json:"success"`
	Error    string `json:"error"`
}

// StartProfilingHandler - POST /minio/admin/v1/profiling/start?profilerType={profilerType}
// ----------
// Enable server profiling
func (a adminAPIHandlers) StartProfilingHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "StartProfiling")

	if globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	vars := mux.Vars(r)
	profiler := vars["profilerType"]

	thisAddr, err := xnet.ParseHost(GetLocalPeer(globalEndpoints))
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Start profiling on remote servers.
	hostErrs := globalNotificationSys.StartProfiling(profiler)

	// Start profiling locally as well.
	{
		if globalProfiler != nil {
			globalProfiler.Stop()
		}
		prof, err := startProfiler(profiler, "")
		if err != nil {
			hostErrs = append(hostErrs, NotificationPeerErr{
				Host: *thisAddr,
				Err:  err,
			})
		} else {
			globalProfiler = prof
			hostErrs = append(hostErrs, NotificationPeerErr{
				Host: *thisAddr,
			})
		}
	}

	var startProfilingResult []StartProfilingResult

	for _, nerr := range hostErrs {
		result := StartProfilingResult{NodeName: nerr.Host.String()}
		if nerr.Err != nil {
			result.Error = nerr.Err.Error()
		} else {
			result.Success = true
		}
		startProfilingResult = append(startProfilingResult, result)
	}

	// Create JSON result and send it to the client
	startProfilingResultInBytes, err := json.Marshal(startProfilingResult)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, []byte(startProfilingResultInBytes))
}

// dummyFileInfo represents a dummy representation of a profile data file
// present only in memory, it helps to generate the zip stream.
type dummyFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
	sys     interface{}
}

func (f dummyFileInfo) Name() string       { return f.name }
func (f dummyFileInfo) Size() int64        { return f.size }
func (f dummyFileInfo) Mode() os.FileMode  { return f.mode }
func (f dummyFileInfo) ModTime() time.Time { return f.modTime }
func (f dummyFileInfo) IsDir() bool        { return f.isDir }
func (f dummyFileInfo) Sys() interface{}   { return f.sys }

// DownloadProfilingHandler - POST /minio/admin/v1/profiling/download
// ----------
// Download profiling information of all nodes in a zip format
func (a adminAPIHandlers) DownloadProfilingHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "DownloadProfiling")

	if globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	if !globalNotificationSys.DownloadProfilingData(ctx, w) {
		writeErrorResponseJSON(w, ErrAdminProfilerNotEnabled, r.URL)
		return
	}
}

// extractHealInitParams - Validates params for heal init API.
func extractHealInitParams(r *http.Request) (bucket, objPrefix string,
	hs madmin.HealOpts, clientToken string, forceStart bool, forceStop bool,
	err APIErrorCode) {

	vars := mux.Vars(r)
	bucket = vars[string(mgmtBucket)]
	objPrefix = vars[string(mgmtPrefix)]

	if bucket == "" {
		if objPrefix != "" {
			// Bucket is required if object-prefix is given
			err = ErrHealMissingBucket
			return
		}
	} else if !IsValidBucketName(bucket) {
		err = ErrInvalidBucketName
		return
	}

	// empty prefix is valid.
	if !IsValidObjectPrefix(objPrefix) {
		err = ErrInvalidObjectName
		return
	}

	qParms := r.URL.Query()
	if len(qParms[string(mgmtClientToken)]) > 0 {
		clientToken = qParms[string(mgmtClientToken)][0]
	}
	if _, ok := qParms[string(mgmtForceStart)]; ok {
		forceStart = true
	}
	if _, ok := qParms[string(mgmtForceStop)]; ok {
		forceStop = true
	}
	// ignore body if clientToken is provided
	if clientToken == "" {
		jerr := json.NewDecoder(r.Body).Decode(&hs)
		if jerr != nil {
			logger.LogIf(context.Background(), jerr)
			err = ErrRequestBodyParse
			return
		}
	}

	err = ErrNone
	return
}

// HealHandler - POST /minio/admin/v1/heal/
// -----------
// Start heal processing and return heal status items.
//
// On a successful heal sequence start, a unique client token is
// returned. Subsequent requests to this endpoint providing the client
// token will receive heal status records from the running heal
// sequence.
//
// If no client token is provided, and a heal sequence is in progress
// an error is returned with information about the running heal
// sequence. However, if the force-start flag is provided, the server
// aborts the running heal sequence and starts a new one.
func (a adminAPIHandlers) HealHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "Heal")

	// Get object layer instance.
	objLayer := newObjectLayerFn()
	if objLayer == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Check if this setup has an erasure coded backend.
	if !globalIsXL {
		writeErrorResponseJSON(w, ErrHealNotImplemented, r.URL)
		return
	}

	bucket, objPrefix, hs, clientToken, forceStart, forceStop, apiErr := extractHealInitParams(r)
	if apiErr != ErrNone {
		writeErrorResponseJSON(w, apiErr, r.URL)
		return
	}

	type healResp struct {
		respBytes []byte
		errCode   APIErrorCode
		errBody   string
	}

	// Define a closure to start sending whitespace to client
	// after 10s unless a response item comes in
	keepConnLive := func(w http.ResponseWriter, respCh chan healResp) {
		ticker := time.NewTicker(time.Second * 10)
		defer ticker.Stop()
		started := false
	forLoop:
		for {
			select {
			case <-ticker.C:
				if !started {
					// Start writing response to client
					started = true
					setCommonHeaders(w)
					w.Header().Set("Content-Type", string(mimeJSON))
					// Set 200 OK status
					w.WriteHeader(200)
				}
				// Send whitespace and keep connection open
				w.Write([]byte("\n\r"))
				w.(http.Flusher).Flush()
			case hr := <-respCh:
				switch hr.errCode {
				case ErrNone:
					if started {
						w.Write(hr.respBytes)
						w.(http.Flusher).Flush()
					} else {
						writeSuccessResponseJSON(w, hr.respBytes)
					}
				default:
					apiError := getAPIError(hr.errCode)
					var errorRespJSON []byte
					if hr.errBody == "" {
						errorRespJSON = encodeResponseJSON(getAPIErrorResponse(apiError, r.URL.Path, w.Header().Get(responseRequestIDKey)))
					} else {
						errorRespJSON = encodeResponseJSON(APIErrorResponse{
							Code:      apiError.Code,
							Message:   hr.errBody,
							Resource:  r.URL.Path,
							RequestID: w.Header().Get(responseRequestIDKey),
							HostID:    "3L137",
						})
					}
					if !started {
						setCommonHeaders(w)
						w.Header().Set("Content-Type", string(mimeJSON))
						w.WriteHeader(apiError.HTTPStatusCode)
					}
					w.Write(errorRespJSON)
					w.(http.Flusher).Flush()
				}
				break forLoop
			}
		}
	}

	// find number of disks in the setup
	info := objLayer.StorageInfo(ctx)
	numDisks := info.Backend.OfflineDisks + info.Backend.OnlineDisks

	healPath := pathJoin(bucket, objPrefix)
	if clientToken == "" && !forceStart && !forceStop {
		nh, exists := globalAllHealState.getHealSequence(healPath)
		if exists && !nh.hasEnded() && len(nh.currentStatus.Items) > 0 {
			b, err := json.Marshal(madmin.HealStartSuccess{
				ClientToken:   nh.clientToken,
				ClientAddress: nh.clientAddress,
				StartTime:     nh.startTime,
			})
			if err != nil {
				writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
				return
			}
			// Client token not specified but a heal sequence exists on a path,
			// Send the token back to client.
			writeSuccessResponseJSON(w, b)
			return
		}
	}

	if clientToken != "" && !forceStart && !forceStop {
		// Since clientToken is given, fetch heal status from running
		// heal sequence.
		respBytes, errCode := globalAllHealState.PopHealStatusJSON(
			healPath, clientToken)
		if errCode != ErrNone {
			writeErrorResponseJSON(w, errCode, r.URL)
		} else {
			writeSuccessResponseJSON(w, respBytes)
		}
		return
	}

	respCh := make(chan healResp)
	switch {
	case forceStop:
		go func() {
			respBytes, errCode := globalAllHealState.stopHealSequence(healPath)
			hr := healResp{respBytes: respBytes, errCode: errCode}
			respCh <- hr
		}()
	case clientToken == "":
		nh := newHealSequence(bucket, objPrefix, handlers.GetSourceIP(r), numDisks, hs, forceStart)
		go func() {
			respBytes, errCode, errMsg := globalAllHealState.LaunchNewHealSequence(nh)
			hr := healResp{respBytes, errCode, errMsg}
			respCh <- hr
		}()
	}

	// Due to the force-starting functionality, the Launch
	// call above can take a long time - to keep the
	// connection alive, we start sending whitespace
	keepConnLive(w, respCh)
}

// GetConfigHandler - GET /minio/admin/v1/config
// Get config.json of this minio setup.
func (a adminAPIHandlers) GetConfigHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetConfigHandler")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	config, err := readServerConfig(ctx, objectAPI)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	configData, err := json.MarshalIndent(config, "", "\t")
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	password := config.GetCredential().SecretKey
	econfigData, err := madmin.EncryptData(password, configData)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, econfigData)
}

// Disable tidwall json array notation in JSON key path so
// users can set json with a key as a number.
// In tidwall json, notify.webhook.0 = val means { "notify" : { "webhook" : [val] }}
// In Minio, notify.webhook.0 = val means { "notify" : { "webhook" : {"0" : val}}}
func normalizeJSONKey(input string) (key string) {
	subKeys := strings.Split(input, ".")
	for i, k := range subKeys {
		if i > 0 {
			key += "."
		}
		if _, err := strconv.Atoi(k); err == nil {
			key += ":" + k
		} else {
			key += k
		}
	}
	return
}

// GetConfigHandler - GET /minio/admin/v1/config-keys
// Get some keys in config.json of this minio setup.
func (a adminAPIHandlers) GetConfigKeysHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "GetConfigKeysHandler")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	var keys []string
	queries := r.URL.Query()

	for k := range queries {
		keys = append(keys, k)
	}

	config, err := readServerConfig(ctx, objectAPI)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	configData, err := json.Marshal(config)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	configStr := string(configData)
	newConfigStr := `{}`

	for _, key := range keys {
		// sjson.Set does not return an error if key is empty
		// we should check by ourselves here
		if key == "" {
			continue
		}
		val := gjson.Get(configStr, key)
		if j, ierr := sjson.Set(newConfigStr, normalizeJSONKey(key), val.Value()); ierr == nil {
			newConfigStr = j
		}
	}

	password := config.GetCredential().SecretKey
	econfigData, err := madmin.EncryptData(password, []byte(newConfigStr))
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, []byte(econfigData))
}

// toAdminAPIErrCode - converts errXLWriteQuorum error to admin API
// specific error.
func toAdminAPIErrCode(ctx context.Context, err error) APIErrorCode {
	switch err {
	case errXLWriteQuorum:
		return ErrAdminConfigNoQuorum
	default:
		return toAPIErrorCode(ctx, err)
	}
}

// RemoveUser - DELETE /minio/admin/v1/remove-user?accessKey=<access_key>
func (a adminAPIHandlers) RemoveUser(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "RemoveUser")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Deny if WORM is enabled
	if globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	vars := mux.Vars(r)
	accessKey := vars["accessKey"]
	if err := globalIAMSys.DeleteUser(accessKey); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
	}
}

// ListUsers - GET /minio/admin/v1/list-users
func (a adminAPIHandlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListUsers")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	allCredentials, err := globalIAMSys.ListUsers()
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	data, err := json.Marshal(allCredentials)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	password := globalServerConfig.GetCredential().SecretKey
	econfigData, err := madmin.EncryptData(password, data)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	writeSuccessResponseJSON(w, econfigData)
}

// SetUserStatus - PUT /minio/admin/v1/set-user-status?accessKey=<access_key>&status=[enabled|disabled]
func (a adminAPIHandlers) SetUserStatus(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "SetUserStatus")

	if globalNotificationSys == nil || globalIAMSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Deny if WORM is enabled
	if globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	vars := mux.Vars(r)
	accessKey := vars["accessKey"]
	status := vars["status"]

	// Custom IAM policies not allowed for admin user.
	if accessKey == globalServerConfig.GetCredential().AccessKey {
		writeErrorResponseJSON(w, ErrInvalidRequest, r.URL)
		return
	}

	if err := globalIAMSys.SetUserStatus(accessKey, madmin.AccountStatus(status)); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Notify all other Minio peers to reload users
	for _, nerr := range globalNotificationSys.LoadUsers() {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// AddUser - PUT /minio/admin/v1/add-user?accessKey=<access_key>
func (a adminAPIHandlers) AddUser(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "AddUser")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil || globalIAMSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Deny if WORM is enabled
	if globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	vars := mux.Vars(r)
	accessKey := vars["accessKey"]

	// Custom IAM policies not allowed for admin user.
	if accessKey == globalServerConfig.GetCredential().AccessKey {
		writeErrorResponseJSON(w, ErrInvalidRequest, r.URL)
		return
	}

	if r.ContentLength > maxEConfigJSONSize || r.ContentLength == -1 {
		// More than maxConfigSize bytes were available
		writeErrorResponseJSON(w, ErrAdminConfigTooLarge, r.URL)
		return
	}

	password := globalServerConfig.GetCredential().SecretKey
	configBytes, err := madmin.DecryptData(password, io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(w, ErrAdminConfigBadJSON, r.URL)
		return
	}

	var uinfo madmin.UserInfo
	if err = json.Unmarshal(configBytes, &uinfo); err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(w, ErrAdminConfigBadJSON, r.URL)
		return
	}

	if err = globalIAMSys.SetUser(accessKey, uinfo); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Notify all other Minio peers to reload users
	for _, nerr := range globalNotificationSys.LoadUsers() {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// ListCannedPolicies - GET /minio/admin/v1/list-canned-policies
func (a adminAPIHandlers) ListCannedPolicies(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "ListCannedPolicies")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalIAMSys == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	policies, err := globalIAMSys.ListCannedPolicies()
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	if err = json.NewEncoder(w).Encode(policies); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	w.(http.Flusher).Flush()
}

// RemoveCannedPolicy - DELETE /minio/admin/v1/remove-canned-policy?name=<policy_name>
func (a adminAPIHandlers) RemoveCannedPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "RemoveCannedPolicy")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalIAMSys == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	vars := mux.Vars(r)
	policyName := vars["name"]

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Deny if WORM is enabled
	if globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	if err := globalIAMSys.DeleteCannedPolicy(policyName); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Notify all other Minio peers to reload users
	for _, nerr := range globalNotificationSys.LoadUsers() {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// AddCannedPolicy - PUT /minio/admin/v1/add-canned-policy?name=<policy_name>
func (a adminAPIHandlers) AddCannedPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "AddCannedPolicy")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalIAMSys == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	vars := mux.Vars(r)
	policyName := vars["name"]

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Deny if WORM is enabled
	if globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	// Error out if Content-Length is missing.
	if r.ContentLength <= 0 {
		writeErrorResponseJSON(w, ErrMissingContentLength, r.URL)
		return
	}

	// Error out if Content-Length is beyond allowed size.
	if r.ContentLength > maxBucketPolicySize {
		writeErrorResponseJSON(w, ErrEntityTooLarge, r.URL)
		return
	}

	iamPolicy, err := iampolicy.ParseConfig(io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		writeErrorResponseJSON(w, ErrMalformedPolicy, r.URL)
		return
	}

	// Version in policy must not be empty
	if iamPolicy.Version == "" {
		writeErrorResponseJSON(w, ErrMalformedPolicy, r.URL)
		return
	}

	if err = globalIAMSys.SetCannedPolicy(policyName, *iamPolicy); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Notify all other Minio peers to reload users
	for _, nerr := range globalNotificationSys.LoadUsers() {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// SetUserPolicy - PUT /minio/admin/v1/set-user-policy?accessKey=<access_key>&name=<policy_name>
func (a adminAPIHandlers) SetUserPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "SetUserPolicy")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalIAMSys == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	vars := mux.Vars(r)
	accessKey := vars["accessKey"]
	policyName := vars["name"]

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Deny if WORM is enabled
	if globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	// Custom IAM policies not allowed for admin user.
	if accessKey == globalServerConfig.GetCredential().AccessKey {
		writeErrorResponseJSON(w, ErrInvalidRequest, r.URL)
		return
	}

	if err := globalIAMSys.SetUserPolicy(accessKey, policyName); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
	}

	// Notify all other Minio peers to reload users
	for _, nerr := range globalNotificationSys.LoadUsers() {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}
}

// SetConfigHandler - PUT /minio/admin/v1/config
func (a adminAPIHandlers) SetConfigHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "SetConfigHandler")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Deny if WORM is enabled
	if globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	if r.ContentLength > maxEConfigJSONSize || r.ContentLength == -1 {
		// More than maxConfigSize bytes were available
		writeErrorResponseJSON(w, ErrAdminConfigTooLarge, r.URL)
		return
	}

	password := globalServerConfig.GetCredential().SecretKey
	configBytes, err := madmin.DecryptData(password, io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(w, ErrAdminConfigBadJSON, r.URL)
		return
	}

	// Validate JSON provided in the request body: check the
	// client has not sent JSON objects with duplicate keys.
	if err = quick.CheckDuplicateKeys(string(configBytes)); err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(w, ErrAdminConfigBadJSON, r.URL)
		return
	}

	var config serverConfig
	if err = json.Unmarshal(configBytes, &config); err != nil {
		logger.LogIf(ctx, err)
		writeCustomErrorResponseJSON(w, ErrAdminConfigBadJSON, err.Error(), r.URL)
		return
	}

	// If credentials for the server are provided via environment,
	// then credentials in the provided configuration must match.
	if globalIsEnvCreds {
		creds := globalServerConfig.GetCredential()
		if config.Credential.AccessKey != creds.AccessKey ||
			config.Credential.SecretKey != creds.SecretKey {
			writeErrorResponseJSON(w, ErrAdminCredentialsMismatch, r.URL)
			return
		}
	}

	if err = config.Validate(); err != nil {
		writeCustomErrorResponseJSON(w, ErrAdminConfigBadJSON, err.Error(), r.URL)
		return
	}

	if err = config.TestNotificationTargets(); err != nil {
		writeCustomErrorResponseJSON(w, ErrAdminConfigBadJSON, err.Error(), r.URL)
		return
	}

	if err = saveServerConfig(ctx, objectAPI, &config); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Reply to the client before restarting minio server.
	writeSuccessResponseHeadersOnly(w)
}

func convertValueType(elem []byte, jsonType gjson.Type) (interface{}, error) {
	str := string(elem)
	switch jsonType {
	case gjson.False, gjson.True:
		return strconv.ParseBool(str)
	case gjson.JSON:
		return gjson.Parse(str).Value(), nil
	case gjson.String:
		return str, nil
	case gjson.Number:
		return strconv.ParseFloat(str, 64)
	default:
		return nil, nil
	}
}

// SetConfigKeysHandler - PUT /minio/admin/v1/config-keys
func (a adminAPIHandlers) SetConfigKeysHandler(w http.ResponseWriter, r *http.Request) {
	ctx := newContext(r, w, "SetConfigKeysHandler")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Deny if WORM is enabled
	if globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	// Validate request signature.
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	// Load config
	configStruct, err := readServerConfig(ctx, objectAPI)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Convert config to json bytes
	configBytes, err := json.Marshal(configStruct)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	configStr := string(configBytes)

	queries := r.URL.Query()
	password := globalServerConfig.GetCredential().SecretKey

	// Set key values in the JSON config
	for k := range queries {
		// Decode encrypted data associated to the current key
		encryptedElem, dErr := base64.StdEncoding.DecodeString(queries.Get(k))
		if dErr != nil {
			reqInfo := (&logger.ReqInfo{}).AppendTags("key", k)
			ctx = logger.SetReqInfo(ctx, reqInfo)
			logger.LogIf(ctx, dErr)
			writeErrorResponseJSON(w, ErrAdminConfigBadJSON, r.URL)
			return
		}
		elem, dErr := madmin.DecryptData(password, bytes.NewBuffer([]byte(encryptedElem)))
		if dErr != nil {
			logger.LogIf(ctx, dErr)
			writeErrorResponseJSON(w, ErrAdminConfigBadJSON, r.URL)
			return
		}
		// Calculate the type of the current key from the
		// original config json
		jsonFieldType := gjson.Get(configStr, k).Type
		// Convert passed value to json filed type
		val, cErr := convertValueType(elem, jsonFieldType)
		if cErr != nil {
			writeCustomErrorResponseJSON(w, ErrAdminConfigBadJSON, cErr.Error(), r.URL)
			return
		}
		// Set the key/value in the new json document
		if s, sErr := sjson.Set(configStr, normalizeJSONKey(k), val); sErr == nil {
			configStr = s
		}
	}

	configBytes = []byte(configStr)

	// Validate config
	var config serverConfig
	if err = json.Unmarshal(configBytes, &config); err != nil {
		writeCustomErrorResponseJSON(w, ErrAdminConfigBadJSON, err.Error(), r.URL)
		return
	}

	if err = config.Validate(); err != nil {
		writeCustomErrorResponseJSON(w, ErrAdminConfigBadJSON, err.Error(), r.URL)
		return
	}

	if err = config.TestNotificationTargets(); err != nil {
		writeCustomErrorResponseJSON(w, ErrAdminConfigBadJSON, err.Error(), r.URL)
		return
	}

	// If credentials for the server are provided via environment,
	// then credentials in the provided configuration must match.
	if globalIsEnvCreds {
		creds := globalServerConfig.GetCredential()
		if config.Credential.AccessKey != creds.AccessKey ||
			config.Credential.SecretKey != creds.SecretKey {
			writeErrorResponseJSON(w, ErrAdminCredentialsMismatch, r.URL)
			return
		}
	}

	if err = saveServerConfig(ctx, objectAPI, &config); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Send success response
	writeSuccessResponseHeadersOnly(w)
}

// UpdateAdminCredsHandler - POST /minio/admin/v1/config/credential
// ----------
// Update admin credentials in a minio server
func (a adminAPIHandlers) UpdateAdminCredentialsHandler(w http.ResponseWriter,
	r *http.Request) {

	ctx := newContext(r, w, "UpdateCredentialsHandler")

	// Get current object layer instance.
	objectAPI := newObjectLayerFn()
	if objectAPI == nil || globalNotificationSys == nil {
		writeErrorResponseJSON(w, ErrServerNotInitialized, r.URL)
		return
	}

	// Avoid setting new credentials when they are already passed
	// by the environment. Deny if WORM is enabled.
	if globalIsEnvCreds || globalWORMEnabled {
		writeErrorResponseJSON(w, ErrMethodNotAllowed, r.URL)
		return
	}

	// Authenticate request
	adminAPIErr := checkAdminRequestAuthType(ctx, r, "")
	if adminAPIErr != ErrNone {
		writeErrorResponseJSON(w, adminAPIErr, r.URL)
		return
	}

	if r.ContentLength > maxEConfigJSONSize || r.ContentLength == -1 {
		// More than maxConfigSize bytes were available
		writeErrorResponseJSON(w, ErrAdminConfigTooLarge, r.URL)
		return
	}

	password := globalServerConfig.GetCredential().SecretKey
	configBytes, err := madmin.DecryptData(password, io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(w, ErrAdminConfigBadJSON, r.URL)
		return
	}

	// Decode request body
	var req madmin.SetCredsReq
	if err = json.Unmarshal(configBytes, &req); err != nil {
		logger.LogIf(ctx, err)
		writeErrorResponseJSON(w, ErrRequestBodyParse, r.URL)
		return
	}

	creds, err := auth.CreateCredentials(req.AccessKey, req.SecretKey)
	if err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Acquire lock before updating global configuration.
	globalServerConfigMu.Lock()
	defer globalServerConfigMu.Unlock()

	// Update local credentials in memory.
	globalServerConfig.SetCredential(creds)

	// Set active creds.
	globalActiveCred = creds

	if err = saveServerConfig(ctx, objectAPI, globalServerConfig); err != nil {
		writeErrorResponseJSON(w, toAdminAPIErrCode(ctx, err), r.URL)
		return
	}

	// Notify all other Minio peers to update credentials
	for _, nerr := range globalNotificationSys.LoadCredentials() {
		if nerr.Err != nil {
			logger.GetReqInfo(ctx).SetTags("peerAddress", nerr.Host.String())
			logger.LogIf(ctx, nerr.Err)
		}
	}

	// Reply to the client before restarting minio server.
	writeSuccessResponseHeadersOnly(w)
}
