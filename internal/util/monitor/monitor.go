/*
 * # Licensed to the LF AI & Data foundation under one
 * # or more contributor license agreements. See the NOTICE file
 * # distributed with this work for additional information
 * # regarding copyright ownership. The ASF licenses this file
 * # to you under the Apache License, Version 2.0 (the
 * # "License"); you may not use this file except in compliance
 * # with the License. You may obtain a copy of the License at
 * #
 * #     http://www.apache.org/licenses/LICENSE-2.0
 * #
 * # Unless required by applicable law or agreed to in writing, software
 * # distributed under the License is distributed on an "AS IS" BASIS,
 * # WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * # See the License for the specific language governing permissions and
 * # limitations under the License.
 */

package monitor

import (
	"expvar"
	"fmt"
	"net/http"
)

var (
	prefix    = "/monitor"
	Proxy     = newMap("proxy")
	RootCoord = newMap("rootcoord")
)

func newMap(name string) *expvar.Map {
	v := new(expvar.Map).Init()
	return v
}

func handler(path string, p *expvar.Map) {
	http.HandleFunc(prefix+path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, p.String())
	})
}

func RegisterMonitor() {
	handler("/proxy", Proxy)
	handler("/rootcoord", RootCoord)
}
