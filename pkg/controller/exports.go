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
	"regexp"
	"strings"
	"time"

	serving "github.com/SENERGY-Platform/analytics-serving/client"
	"github.com/SENERGY-Platform/cost-calculator/pkg/model"
	"github.com/SENERGY-Platform/models/go/models"
	prometheus_model "github.com/prometheus/common/model"
)

/*	Limitations:
	- Export cost only considers storage cost.
*/

var exportTableMatch = regexp.MustCompile("userid:(.{22})_export:(.{22}).*")

func (c *Controller) GetExportsTree(userId string, token string, admin bool, skipEstimation bool) (result model.CostWithChildren, err error) {
	timer := time.Now()
	result = model.CostWithChildren{
		CostWithEstimation: model.CostWithEstimation{},
		Children:           map[string]model.CostWithChildren{},
	}

	hoursInMonthProgressed, timeInMonthRemaining, hoursInMonthProgressedStr, secondsInMonthRemainingStr, _ := getMonthTimeInfo()
	var instances serving.Instances

	t := true
	options := serving.ListOptions{
		InternalOnly: &t,
	}
	if !admin {
		resp, err := c.servingClient.ListInstances(token, &options)
		if err != nil {
			return result, err
		}
		instances = resp.Instances
	} else {
		instances, err = c.servingClient.ListInstancesAsAdmin(token, &options)
		if err != nil {
			return
		}
	}

	shortUserId, err := models.ShortenId(userId)
	if err != nil {
		return result, err
	}

	tables := []string{}

	for _, instance := range instances {
		if instance.UserId != userId || instance.ExportDatabase.Url != c.config.ServingTimescaleConfiguredUrl {
			continue
		}
		id := instance.ID.String()

		shortId, err := models.ShortenId(id)
		if err != nil {
			return result, err
		}

		tables = append(tables, "userid:"+shortUserId+"_export:"+shortId)
	}

	tableSizeByteMap := map[string]float64{}

	insertWithQuery := func(promQuery string, estimation bool) error {
		resp, w, err := c.prometheus.Query(context.Background(), promQuery, time.Now())
		if err != nil {
			return err
		}
		if len(w) > 0 {
			log.Printf("WARNING: prometheus warnings = %#v\n", w)
		}
		if resp.Type() != prometheus_model.ValVector {
			return fmt.Errorf("unexpected prometheus response %#v", resp)
		}
		values, ok := resp.(prometheus_model.Vector)
		if !ok {
			return fmt.Errorf("unexpected prometheus response %#v", resp)
		}

		for _, element := range values {
			table, ok := element.Metric["table"]
			if !ok {
				return fmt.Errorf("unexpected prometheus response element %#v", element)
			}

			matches := exportTableMatch.FindAllStringSubmatch(string(table), -1)
			if matches == nil || len(matches[0]) != 3 {
				return fmt.Errorf("received metric for unexpected table name %#v", table)
			}

			exportId, err := models.LongId(matches[0][2])
			if err != nil {
				return err
			}

			tableSizeBytes := sampleToFloat(element.Value)
			child, ok := result.Children[string(exportId)]
			if !ok {
				child = model.CostWithChildren{
					CostWithEstimation: model.CostWithEstimation{
						Month:           model.CostEntry{},
						EstimationMonth: model.CostEntry{},
					},
				}
			}
			if estimation {
				tableSizeBytesEstimation := tableSizeBytes
				tableSizeBytes, ok := tableSizeByteMap[exportId]
				if !ok {
					tableSizeBytes = 0
				}

				avgFutureTableSize := (tableSizeBytesEstimation + tableSizeBytes) / 2
				futureCost := c.pricingModel.Storage * avgFutureTableSize * timeInMonthRemaining.Hours() / 1000000000 // cost * avg-size * hours-progressed / correction-bytes-in-gb
				child.CostWithEstimation.EstimationMonth.Storage = child.CostWithEstimation.Month.Storage + futureCost
				result.CostWithEstimation.EstimationMonth.Storage += child.EstimationMonth.Storage
			} else {
				tableSizeByteMap[exportId] = tableSizeBytes
				child.CostWithEstimation.Month.Storage = c.pricingModel.Storage * tableSizeBytes * float64(hoursInMonthProgressed) / 1000000000 // cost * avg-size * hours-progressed / correction-bytes-in-gb
				result.CostWithEstimation.Month.Storage += child.Month.Storage
			}
			result.Children[exportId] = child
		}
		return nil
	}
	// Costs in current month
	promQuery := "avg_over_time(avg by (table) (timescale_table_size_bytes{table=~\"" + strings.Join(tables, "|") + "\"})[" + hoursInMonthProgressedStr + "h:])"
	err = insertWithQuery(promQuery, false)
	if err != nil {
		return result, err
	}

	// Estimations
	if !skipEstimation {
		promQuery = "predict_linear(avg by (table) (timescale_table_size_bytes{table=~\"" + strings.Join(tables, "|") + "\"})[24h:], " + secondsInMonthRemainingStr + ")"
		err = insertWithQuery(promQuery, true)
		if err != nil {
			return result, err
		}
	}
	c.logDebug("ExportsTree " + time.Since(timer).String())
	return
}
