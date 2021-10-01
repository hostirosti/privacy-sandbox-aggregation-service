// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This binary aggregates the partial report for each aggregation ID, which is calculated from the exponentiated conversion keys from the other helper.
// The pipeline can be executed in two ways:
//
// 1. Directly on local
// /path/to/dpf_aggregate_partial_report \
// --partial_report_file=/path/to/partial_report_file.txt \
// --partial_histogram_file=/path/to/partial_histogram_file.txt \
// --private_key_dir=/path/to/private_key_dir \
// --runner=direct
//
// 2. Dataflow on cloud
// /path/to/dpf_aggregate_partial_report \
// --partial_report_file=gs://<helper bucket>/partial_report_file.txt \
// --partial_histogram_file=gs://<helper bucket>/partial_histogram_file.txt \
// --private_key_dir=/path/to/private_key_dir \
// --runner=dataflow \
// --project=<GCP project> \
// --temp_location=gs://<dataflow temp dir> \
// --staging_location=gs://<dataflow temp dir> \
// --worker_binary=/path/to/dpf_aggregate_partial_report
package main

import (
	"context"
	"flag"
	"math"

	"github.com/apache/beam/sdks/go/pkg/beam"
	"github.com/apache/beam/sdks/go/pkg/beam/log"
	"github.com/apache/beam/sdks/go/pkg/beam/x/beamx"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/cryptoio"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/dpfaggregator"

	pb "github.com/google/privacy-sandbox-aggregation-service/pipeline/crypto_go_proto"
)

var (
	partialReportURI    = flag.String("partial_report_uri", "", "Input partial reports. It may contain the original encrypted partial reports or evaluation context.")
	expandParametersURI = flag.String("expand_parameters_uri", "", "Input URI of the expansion parameter file.")
	partialHistogramURI = flag.String("partial_histogram_uri", "", "Output partial aggregation.")
	decryptedReportURI  = flag.String("decrypted_report_uri", "", "Output the decrypted partial reports so the helper won't need to do the decryption repeatedly.")
	keyBitSize          = flag.Int("key_bit_size", 32, "Bit size of the conversion keys.")
	privateKeyParamsURI = flag.String("private_key_params_uri", "", "Input file that stores the parameters required to read the standard private keys.")

	directCombine = flag.Bool("direct_combine", true, "Use direct or segmented combine when aggregating the expanded vectors.")
	segmentLength = flag.Uint64("segment_length", 32768, "Segment length to split the original vectors.")

	epsilon = flag.Float64("epsilon", 0.0, "Epsilon for the privacy budget.")
	// The default l1 sensitivity is consistent with:
	// https://github.com/WICG/conversion-measurement-api/blob/main/AGGREGATE.md#privacy-budgeting
	l1Sensitivity = flag.Uint64("l1_sensitivity", uint64(math.Pow(2, 16)), "L1-sensitivity for the privacy budget.")

	fileShards = flag.Int64("file_shards", 1, "The number of shards for the output file.")

	// The following flags will be retired and are kept for now to make the pipeline compatible with the current server implementation.
	sumParametersURI = flag.String("sum_parameters_uri", "", "Input file that stores the DPF parameters for sum.")
	prefixesURI      = flag.String("prefixes_uri", "", "Input file that stores the prefixes for hierarchical DPF expansion.")
)

func main() {
	flag.Parse()
	beam.Init()

	ctx := context.Background()
	helperPrivKeys, err := cryptoio.ReadPrivateKeyCollection(ctx, *privateKeyParamsURI)
	if err != nil {
		log.Exit(ctx, err)
	}

	var expandParams *pb.ExpandParameters
	if *expandParametersURI != "" {
		expandParams, err = cryptoio.ReadExpandParameters(ctx, *expandParametersURI)
		if err != nil {
			log.Exit(ctx, err)
		}
	} else {
		sumParams, err := cryptoio.ReadDPFParameters(ctx, *sumParametersURI)
		if err != nil {
			log.Exit(ctx, err)
		}
		prefixes, err := cryptoio.ReadPrefixes(ctx, *prefixesURI)
		if err != nil {
			log.Exit(ctx, err)
		}
		expandParams, err = dpfaggregator.ConvertOldParamsToExpandParameter(sumParams, prefixes)
		if err != nil {
			log.Exit(ctx, err)
		}
	}

	// For the current design, we define hierarchies for all possible prefix lengths of the bucket ID.
	params, err := dpfaggregator.GetDefaultDPFParameters(int32(*keyBitSize))
	if err != nil {
		log.Exit(ctx, err)
	}
	expandParams.SumParameters = &pb.IncrementalDpfParameters{
		Params: params,
	}

	pipeline := beam.NewPipeline()
	scope := pipeline.Root()
	if err := dpfaggregator.AggregatePartialReport(
		scope,
		&dpfaggregator.AggregatePartialReportParams{
			PartialReportURI:    *partialReportURI,
			PartialHistogramURI: *partialHistogramURI,
			DecryptedReportURI:  *decryptedReportURI,
			HelperPrivateKeys:   helperPrivKeys,
			ExpandParams:        expandParams,
			CombineParams: &dpfaggregator.CombineParams{
				DirectCombine: *directCombine,
				SegmentLength: *segmentLength,
				Epsilon:       *epsilon,
				L1Sensitivity: *l1Sensitivity,
			},
			Shards:               *fileShards,
			UseEvaluationContext: *expandParametersURI != "",
		}); err != nil {
		log.Exit(ctx, err)
	}
	if err := beamx.Run(ctx, pipeline); err != nil {
		log.Exitf(ctx, "Failed to execute job: %s", err)
	}
}
