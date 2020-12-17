/*
Copyright © 2020 Luke Hinds <lhinds@redhat.com>

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/projectrekor/rekor/pkg/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/google/trillian"
	"github.com/google/trillian/client"
	"github.com/google/trillian/crypto/keyspb"
	"github.com/google/trillian/crypto/sigpb"
	"github.com/google/trillian/merkle"
	"github.com/google/trillian/merkle/rfc6962"
	"github.com/google/trillian/types"
)

type TrillianClient struct {
	client trillian.TrillianLogClient
	logID  int64
}

type Response struct {
	status                    codes.Code
	getAddResult              *trillian.QueueLeafResponse
	getLeafResult             *trillian.GetLeavesByHashResponse
	getProofResult            *trillian.GetInclusionProofByHashResponse
	getLeafByIndexResult      *trillian.GetLeavesByIndexResponse
	getLatestResult           *trillian.GetLatestSignedLogRootResponse
	getConsistencyProofResult *trillian.GetConsistencyProofResponse
}

func TrillianClientInstance(client trillian.TrillianLogClient, tLogID int64) *TrillianClient {
	return &TrillianClient{
		client: client,
		logID:  tLogID,
	}
}

func (t *TrillianClient) root() (types.LogRootV1, error) {
	rqst := &trillian.GetLatestSignedLogRootRequest{
		LogId: t.logID,
	}
	resp, err := t.client.GetLatestSignedLogRoot(context.Background(), rqst)
	if err != nil {
		return types.LogRootV1{}, err
	}
	var root types.LogRootV1
	if err := root.UnmarshalBinary(resp.SignedLogRoot.LogRoot); err != nil {
		return types.LogRootV1{}, err
	}
	return root, nil
}

func (t *TrillianClient) addLeaf(byteValue []byte) (*Response, error) {
	leaf := &trillian.LogLeaf{
		LeafValue: byteValue,
	}
	rqst := &trillian.QueueLeafRequest{
		LogId: t.logID,
		Leaf:  leaf,
	}
	resp, err := t.client.QueueLeaf(context.Background(), rqst)
	if err != nil {
		fmt.Println(err)
	}

	return &Response{
		status:       codes.Code(resp.QueuedLeaf.GetStatus().GetCode()),
		getAddResult: resp,
	}, nil
}

func (t *TrillianClient) getLeafByHash(hashValues [][]byte) (*Response, error) {
	rqst := &trillian.GetLeavesByHashRequest{
		LogId:    t.logID,
		LeafHash: hashValues,
	}

	resp, err := t.client.GetLeavesByHash(context.Background(), rqst)
	if err != nil {
		log.Logger.Fatal(err)
	}

	return &Response{
		status:        status.Code(err),
		getLeafResult: resp,
	}, nil
}

func (t *TrillianClient) getLeafByIndex(indexes []int64) (*Response, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := t.client.GetLeavesByIndex(ctx,
		&trillian.GetLeavesByIndexRequest{
			LogId:     t.logID,
			LeafIndex: indexes,
		})

	return &Response{
		status:               status.Code(err),
		getLeafByIndexResult: resp,
	}, nil
}

func (t *TrillianClient) getProofByHash(hashValue []byte) (*Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	root, err := t.root()
	if err != nil {
		return &Response{}, err
	}

	resp, err := t.client.GetInclusionProofByHash(ctx,
		&trillian.GetInclusionProofByHashRequest{
			LogId:    t.logID,
			LeafHash: hashValue,
			TreeSize: int64(root.TreeSize),
		})

	v := merkle.NewLogVerifier(rfc6962.DefaultHasher)

	if resp != nil {
		for i, proof := range resp.Proof {
			hashes := proof.GetHashes()
			for j, hash := range hashes {
				log.Logger.Infof("Proof[%d],hash[%d] == %x\n", i, j, hash)
			}
			if err := v.VerifyInclusionProof(proof.LeafIndex, int64(root.TreeSize), hashes, root.RootHash, hashValue); err != nil {
				return &Response{}, err
			}
		}
	}

	return &Response{
		status:         status.Code(err),
		getProofResult: resp,
	}, nil
}

func (t *TrillianClient) getLatest(leafSizeInt int64) (*Response, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := t.client.GetLatestSignedLogRoot(ctx,
		&trillian.GetLatestSignedLogRootRequest{
			LogId:         t.logID,
			FirstTreeSize: leafSizeInt,
		})
	if err != nil {
		return nil, err
	}

	return &Response{
		status:          status.Code(err),
		getLatestResult: resp,
	}, nil
}

func (t *TrillianClient) getConsistencyProof(firstSize, lastSize int64) (*Response, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := t.client.GetConsistencyProof(ctx,
		&trillian.GetConsistencyProofRequest{
			LogId:          t.logID,
			FirstTreeSize:  firstSize,
			SecondTreeSize: lastSize,
		})
	if err != nil {
		return nil, err
	}

	return &Response{
		status:                    status.Code(err),
		getConsistencyProofResult: resp,
	}, nil
}

func createAndInitTree(ctx context.Context, adminClient trillian.TrillianAdminClient, logClient trillian.TrillianLogClient) (*trillian.Tree, error) {
	// First look for and use an existing tree
	trees, err := adminClient.ListTrees(ctx, &trillian.ListTreesRequest{})
	if err != nil {
		return nil, err
	}

	for _, t := range trees.Tree {
		if t.TreeType == trillian.TreeType_LOG {
			return t, nil
		}
	}

	// Otherwise create and initialize one
	t, err := adminClient.CreateTree(ctx, &trillian.CreateTreeRequest{
		Tree: &trillian.Tree{
			TreeType:           trillian.TreeType_LOG,
			HashStrategy:       trillian.HashStrategy_RFC6962_SHA256,
			HashAlgorithm:      sigpb.DigitallySigned_SHA256,
			SignatureAlgorithm: sigpb.DigitallySigned_ECDSA,
			TreeState:          trillian.TreeState_ACTIVE,
			MaxRootDuration:    ptypes.DurationProto(time.Hour),
		},
		KeySpec: &keyspb.Specification{
			Params: &keyspb.Specification_EcdsaParams{
				EcdsaParams: &keyspb.Specification_ECDSA{},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	if err := client.InitLog(ctx, t, logClient); err != nil {
		return nil, err
	}
	return t, nil
}