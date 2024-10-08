/*
 *    Copyright 2024 InfAI (CC SES)
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package controller

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/SENERGY-Platform/cost-calculator/pkg/model"
	prometheus_model "github.com/prometheus/common/model"
)

func (c *Controller) GetApiCallsTree(username string, skipEstimation bool, start *time.Time, end *time.Time) (result model.CostWithChildren, err error) {
	timer := time.Now()

	if (start == nil && end != nil) || (start != nil && end == nil) || (start != nil && !skipEstimation) {
		return result, fmt.Errorf("must not provide only one of start or end. must not provide start and stop without skipEstimation")
	}
	if start == nil {
		start, end = defaultStartEnd()
	}
	result = model.CostWithChildren{
		CostWithEstimation: model.CostWithEstimation{
			EstimationMonth: model.CostEntry{},
			Month:           model.CostEntry{},
		},
		Children: map[string]model.CostWithChildren{},
	}
	nextMonth := time.Date(time.Now().Year(), time.Now().Month()+1, 0, 0, 0, 0, 0, time.UTC) // this is okay, because multiplier is only used in estimations, and estimations with start and stop set are not allowed
	multiplier := 1 / (float64(end.Sub(*start)) / float64(nextMonth.Sub(*start)))

	clientPrefix := username + "_"
	query := "round(sum by (exported_service, consumer) (increase(kong_http_requests_total{consumer=~\"" + clientPrefix + ".*\"}[" + end.Sub(*start).Round(time.Second).String() + "]))) != 0"

	resp, w, err := c.prometheus.Query(context.Background(), query, *end)
	if err != nil {
		return result, err
	}
	if len(w) > 0 {
		log.Printf("WARNING: prometheus warnings = %#v\n", w)
	}
	if resp.Type() != prometheus_model.ValVector {
		return result, fmt.Errorf("unexpected prometheus response %#v", resp)
	}
	values, ok := resp.(prometheus_model.Vector)
	if !ok {
		return result, fmt.Errorf("unexpected prometheus response %#v", resp)
	}

	for _, element := range values {
		client := ""
		service := ""
		for _, metricLabel := range element.Metric {
			label := string(metricLabel)
			if strings.HasPrefix(label, clientPrefix) {
				client = strings.TrimPrefix(label, clientPrefix)
			} else {
				service = label
			}
		}
		clientEntry, ok := result.Children[client]
		if !ok {
			clientEntry = model.CostWithChildren{
				CostWithEstimation: model.CostWithEstimation{
					Month: model.CostEntry{},
				},
				Children: map[string]model.CostWithChildren{},
			}
		}
		serviceEntry, ok := clientEntry.Children[service]
		if !ok {
			serviceEntry = model.CostWithChildren{
				CostWithEstimation: model.CostWithEstimation{
					Month: model.CostEntry{},
				},
			}
		}
		value := sampleToFloat(element.Value)

		clientEntry.CostWithEstimation.Month.Requests += value

		serviceEntry.CostWithEstimation.Month.Requests += value

		result.Month.Requests += value

		if !skipEstimation {
			estimate := math.Round(value * multiplier)
			clientEntry.CostWithEstimation.EstimationMonth = model.CostEntry{}
			clientEntry.CostWithEstimation.EstimationMonth.Requests += estimate
			serviceEntry.CostWithEstimation.EstimationMonth = model.CostEntry{}
			serviceEntry.CostWithEstimation.EstimationMonth.Requests += estimate
			result.EstimationMonth.Requests += estimate
		}

		clientEntry.Children[service] = serviceEntry
		result.Children[client] = clientEntry
	}
	c.logDebug("ApiCallsTree " + time.Since(timer).String())

	return
}
