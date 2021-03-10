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

package browsersimulator

import (
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	"github.com/apache/beam/sdks/go/pkg/beam"
	"github.com/apache/beam/sdks/go/pkg/beam/testing/passert"
	"github.com/apache/beam/sdks/go/pkg/beam/testing/ptest"
	"github.com/google/go-cmp/cmp"
	"github.com/pborman/uuid"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/conversion"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/conversionaggregator"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/cryptoio"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/elgamalencrypt"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/secretshare"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/standardencrypt"

	pb "github.com/google/privacy-sandbox-aggregation-service/pipeline/crypto_go_proto"
)

func TestSplitIntoByteShares(t *testing.T) {
	a := "abcd"
	split11, split12, err := SplitIntoByteShares([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	split21, split22, err := SplitIntoByteShares([]byte(a))
	if err != nil {
		t.Fatal(err)
	}

	if cmp.Equal(split11, split21) || cmp.Equal(split11, split22) || cmp.Equal(split12, split21) || cmp.Equal(split12, split22) {
		t.Errorf("expect random splits, got identical ones")
	}

	combine, err := secretshare.CombineByteShares(split11, split12)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(a, string(combine)); diff != "" {
		t.Errorf("combined result mismatch (-want +got):\n%s", diff)
	}
}

func TestCreateRandomUniqueID(t *testing.T) {
	id1 := createRandomReportID()
	id2 := createRandomReportID()
	if id1 == id2 {
		t.Fatalf("IDs should be unique and random")
	}
}

func prepareKeys(helper string) (privKeyDir, pubKeyDir string, err error) {
	privKeyDir, err = ioutil.TempDir("/tmp", helper+"_priv")
	if err != nil {
		return
	}

	sPub, ePub, err := cryptoio.CreateKeysAndSecret(privKeyDir)
	if err != nil {
		return
	}

	pubKeyDir, err = ioutil.TempDir("/tmp", helper+"_pub")
	if err != nil {
		return
	}

	err = cryptoio.SaveStandardPublicKey(pubKeyDir, sPub)
	if err != nil {
		return
	}
	err = cryptoio.SaveElGamalPublicKey(pubKeyDir, ePub)
	return
}

func mergeReportFn(reportID string, prIter1, prIter2 func(**pb.PartialReport) bool, emit func(rawConversion)) error {
	var pr1, pr2 *pb.PartialReport
	if !prIter1(&pr1) {
		return fmt.Errorf("missing partial report for helper 1")
	}
	if !prIter2(&pr2) {
		return fmt.Errorf("missing partial report for helper 2")
	}

	key, err := secretshare.CombineByteShares(pr1.GetKeyShare(), pr2.GetKeyShare())
	if err != nil {
		return err
	}

	emit(rawConversion{
		Key: string(key),
		// The two shares are generated by function:
		// http://google3/chrome/privacy_sandbox/potassium_aggregation_infra/browsersimulator.go?l=36&rcl=341441662
		// So the combined value should be a valid uint16 integer.
		Value: uint16(secretshare.CombineIntShares(pr1.GetValueShare(), pr2.GetValueShare())),
	})
	return nil
}

func TestSplitAndEncryption(t *testing.T) {
	helperPriv1, helperPub1, err := prepareKeys("helper1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(helperPriv1)
	defer os.RemoveAll(helperPub1)

	helperPriv2, helperPub2, err := prepareKeys("helper2")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(helperPriv2)
	defer os.RemoveAll(helperPub2)

	pubInfo1, err := GetPublicInfo(helperPub1)
	if err != nil {
		t.Fatal(err)
	}

	pubInfo2, err := GetPublicInfo(helperPub2)
	if err != nil {
		t.Fatal(err)
	}

	privInfo1, err := conversion.GetPrivateInfo(helperPriv1)
	if err != nil {
		t.Fatal(err)
	}

	privInfo2, err := conversion.GetPrivateInfo(helperPriv2)
	if err != nil {
		t.Fatal(err)
	}

	pipeline, scope := beam.NewPipelineWithRoot()

	lines := beam.CreateList(scope, []string{
		"foo,1",
		"bar,2",
	})
	rawConversions := beam.ParDo(scope, &parseRawConversionFn{}, lines)
	pr1, pr2 := splitRawConversion(scope, rawConversions, pubInfo1, pubInfo2)

	prDecrypted1 := conversion.DecryptPartialReport(scope, pr1, privInfo1.StandardPrivateKey)
	prDecrypted2 := conversion.DecryptPartialReport(scope, pr2, privInfo2.StandardPrivateKey)

	joined := beam.CoGroupByKey(scope, prDecrypted1, prDecrypted2)
	conversions := beam.ParDo(scope, mergeReportFn, joined)

	passert.Equals(scope, conversions, rawConversion{Key: "foo", Value: 1}, rawConversion{Key: "bar", Value: 2})

	if err := ptest.Run(pipeline); err != nil {
		t.Fatalf("pipeline failed: %s", err)
	}
}

func createServerInfo() (*conversion.ServerPrivateInfo, *ServerPublicInfo, error) {
	ePriv, ePub, err := elgamalencrypt.GenerateElGamalKeyPair()
	if err != nil {
		return nil, nil, err
	}
	secret, err := elgamalencrypt.GenerateSecret()
	if err != nil {
		return nil, nil, err
	}

	sPriv, sPub, err := standardencrypt.GenerateStandardKeyPair()
	if err != nil {
		return nil, nil, err
	}
	return &conversion.ServerPrivateInfo{
			ElGamalPrivateKey:  ePriv,
			StandardPrivateKey: sPriv,
			Secret:             secret,
		}, &ServerPublicInfo{
			ElGamalPublicKey:  ePub,
			StandardPublicKey: sPub,
		}, nil
}

func createConversions(n int) ([]rawConversion, []conversionaggregator.CompleteResult) {
	var conversions []rawConversion
	var results []conversionaggregator.CompleteResult
	for i := 0; i < (n+9)/10; i++ {
		key := uuid.New()
		sum := 0
		for j := 0; j < n/10; j++ {
			if len(conversions) >= n {
				continue
			}
			conversions = append(conversions, rawConversion{Key: key, Value: uint16(1)})
			sum++
		}
		results = append(results, conversionaggregator.CompleteResult{ConversionKey: key, Sum: uint32(sum), Count: int64(sum * 2)})

		if len(conversions) >= n {
			break
		}
	}
	return conversions, results
}

func TestAggregationPipeline(t *testing.T) {
	testAggregationPipeline(t)
}

func testAggregationPipeline(t testing.TB) {
	privInfo1, pubInfo1, err := createServerInfo()
	if err != nil {
		t.Fatal(err)
	}
	privInfo2, pubInfo2, err := createServerInfo()
	if err != nil {
		t.Fatal(err)
	}

	conversionArray, wantArray := createConversions(100)

	beam.Init()
	pipeline, scope := beam.NewPipelineWithRoot()

	conversions := beam.CreateList(scope, conversionArray)
	want := beam.CreateList(scope, wantArray)
	ePr1, ePr2 := splitRawConversion(scope, conversions, pubInfo1, pubInfo2)

	pr1 := conversion.DecryptPartialReport(scope, ePr1, privInfo1.StandardPrivateKey)
	pr2 := conversion.DecryptPartialReport(scope, ePr2, privInfo2.StandardPrivateKey)

	idKey1 := conversion.ExponentiateKey(scope, pr1, privInfo1.Secret, pubInfo2.ElGamalPublicKey)
	idKey2 := conversion.ExponentiateKey(scope, pr2, privInfo2.Secret, pubInfo1.ElGamalPublicKey)

	idKeyShare1, aggData1 := conversion.RekeyByAggregationID(scope, idKey2, pr1, privInfo1.ElGamalPrivateKey, privInfo1.Secret)
	idKeyShare2, aggData2 := conversion.RekeyByAggregationID(scope, idKey1, pr2, privInfo2.ElGamalPrivateKey, privInfo2.Secret)

	pAgg1 := conversionaggregator.AggregateDataShare(scope, idKeyShare1, aggData1, true /*ignorePrivacy*/, conversionaggregator.PrivacyParams{})
	pAgg2 := conversionaggregator.AggregateDataShare(scope, idKeyShare2, aggData2, true /*ignorePrivacy*/, conversionaggregator.PrivacyParams{})

	got := conversionaggregator.MergeAggregation(scope, pAgg1, pAgg2)

	passert.Equals(scope, got, want)

	if err := ptest.Run(pipeline); err != nil {
		t.Fatalf("pipeline failed: %s", err)
	}
}

func BenchmarkPipeline(b *testing.B) {
	for i := 0; i < b.N; i++ {
		testAggregationPipeline(b)
	}
}
