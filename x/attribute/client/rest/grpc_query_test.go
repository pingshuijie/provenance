package rest_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/suite"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256r1"
	sdktestutil "github.com/cosmos/cosmos-sdk/testutil"
	testnet "github.com/cosmos/cosmos-sdk/testutil/network"
	sdk "github.com/cosmos/cosmos-sdk/types"
	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
	"github.com/cosmos/cosmos-sdk/types/query"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/gogoproto/proto"

	"github.com/provenance-io/provenance/internal/pioconfig"
	"github.com/provenance-io/provenance/testutil"
	attributetypes "github.com/provenance-io/provenance/x/attribute/types"
	nametypes "github.com/provenance-io/provenance/x/name/types"
)

type IntegrationTestSuite struct {
	suite.Suite

	cfg     testnet.Config
	testnet *testnet.Network

	accountAddr sdk.AccAddress
	accountKey  *secp256r1.PrivKey
	accountStr  string
}

func (s *IntegrationTestSuite) SetupSuite() {
	privKey, _ := secp256r1.GenPrivKey()
	s.accountKey = privKey
	addr, err := sdk.AccAddressFromHexUnsafe(s.accountKey.PubKey().Address().String())
	s.Require().NoError(err)
	s.accountAddr = addr
	s.accountStr = s.accountAddr.String()
	s.T().Log("setting up integration test suite")
	pioconfig.SetProvConfig("atom")

	cfg := testutil.DefaultTestNetworkConfig()

	genesisState := cfg.GenesisState
	cfg.NumValidators = 1

	var authData authtypes.GenesisState
	s.Require().NoError(cfg.Codec.UnmarshalJSON(genesisState[authtypes.ModuleName], &authData))
	genAccount, err := codectypes.NewAnyWithValue(&authtypes.BaseAccount{
		Address:       s.accountAddr.String(),
		AccountNumber: 1,
		Sequence:      0,
	})
	s.Require().NoError(err)
	authData.Accounts = append(authData.Accounts, genAccount)

	// Configure Genesis data for name module
	attrModAddr := authtypes.NewModuleAddress(attributetypes.ModuleName)
	var nameData nametypes.GenesisState
	nameData.Bindings = append(nameData.Bindings, nametypes.NewNameRecord("attribute", s.accountAddr, true))
	nameData.Bindings = append(nameData.Bindings, nametypes.NewNameRecord("example.attribute", s.accountAddr, false))
	nameData.Bindings = append(nameData.Bindings, nametypes.NewNameRecord(attributetypes.AccountDataName, attrModAddr, true))
	nameData.Params.AllowUnrestrictedNames = false
	nameData.Params.MaxNameLevels = 3
	nameData.Params.MinSegmentLength = 3
	nameData.Params.MaxSegmentLength = 12
	nameDataBz, err := cfg.Codec.MarshalJSON(&nameData)
	s.Require().NoError(err)
	genesisState[nametypes.ModuleName] = nameDataBz

	// Configure Genesis data for account module
	var accountData attributetypes.GenesisState
	accountData.Attributes = append(accountData.Attributes,
		attributetypes.NewAttribute(
			"example.attribute",
			s.accountStr,
			attributetypes.AttributeType_String,
			[]byte("example attribute value string"),
			nil, ""),
		attributetypes.NewAttribute(
			attributetypes.AccountDataName,
			s.accountStr,
			attributetypes.AttributeType_String,
			[]byte("example accountdata value string"),
			nil, ""),
	)
	accountData.Params.MaxValueLength = 32
	accountDataBz, err := cfg.Codec.MarshalJSON(&accountData)
	s.Require().NoError(err)
	genesisState[attributetypes.ModuleName] = accountDataBz

	cfg.GenesisState = genesisState

	s.cfg = cfg
	s.testnet, err = testnet.New(s.T(), s.T().TempDir(), cfg)
	s.Require().NoError(err, "creating testnet")

	_, err = testutil.WaitForHeight(s.testnet, 1)
	s.Require().NoError(err, "waiting for height 1")
}

func (s *IntegrationTestSuite) TearDownSuite() {
	testutil.Cleanup(s.testnet, s.T())
}

func (s *IntegrationTestSuite) TestGRPCQueries() {
	val := s.testnet.Validators[0]
	baseURL := val.APIAddress

	testCases := []struct {
		name     string
		url      string
		headers  map[string]string
		expErr   bool
		respType proto.Message
		expected proto.Message
	}{
		{
			"get attribute params",
			fmt.Sprintf("%s/provenance/attribute/v1/params", baseURL),
			map[string]string{
				grpctypes.GRPCBlockHeightHeader: "1",
			},
			false,
			&attributetypes.QueryParamsResponse{},
			&attributetypes.QueryParamsResponse{Params: attributetypes.NewParams(32)},
		},
		{
			"get account attributes",
			fmt.Sprintf("%s/provenance/attribute/v1/attributes/%s", baseURL, s.accountAddr),
			map[string]string{},
			false,
			&attributetypes.QueryAttributesResponse{},
			&attributetypes.QueryAttributesResponse{
				Account: s.accountAddr.String(),
				Attributes: []attributetypes.Attribute{
					attributetypes.NewAttribute("example.attribute",
						s.accountStr,
						attributetypes.AttributeType_String,
						[]byte("example attribute value string"),
						nil, ""),
					attributetypes.NewAttribute(
						attributetypes.AccountDataName,
						s.accountStr,
						attributetypes.AttributeType_String,
						[]byte("example accountdata value string"),
						nil, ""),
				},
				Pagination: &query.PageResponse{NextKey: nil, Total: 2},
			},
		},
		{
			"get account attribute by name",
			fmt.Sprintf("%s/provenance/attribute/v1/attribute/%s/%s", baseURL, s.accountAddr, "example.attribute"),
			map[string]string{},
			false,
			&attributetypes.QueryAttributeResponse{},
			&attributetypes.QueryAttributeResponse{
				Account: s.accountAddr.String(),
				Attributes: []attributetypes.Attribute{
					attributetypes.NewAttribute("example.attribute",
						s.accountStr,
						attributetypes.AttributeType_String,
						[]byte("example attribute value string"),
						nil,
						"",
					),
				},
				Pagination: &query.PageResponse{NextKey: nil, Total: 1},
			},
		},
		{
			"get non existint account attribute by name",
			fmt.Sprintf("%s/provenance/attribute/v1/attribute/%s/%s", baseURL, s.accountAddr, "im.not.here.attribute"),
			map[string]string{},
			false,
			&attributetypes.QueryAttributeResponse{},
			&attributetypes.QueryAttributeResponse{
				Account:    s.accountAddr.String(),
				Attributes: nil,
				Pagination: &query.PageResponse{},
			},
		},
		{
			"scan account attribute by suffix",
			fmt.Sprintf("%s/provenance/attribute/v1/attribute/%s/scan/%s", baseURL, s.accountAddr, "attribute"),
			map[string]string{},
			false,
			&attributetypes.QueryScanResponse{},
			&attributetypes.QueryScanResponse{
				Account: s.accountAddr.String(),
				Attributes: []attributetypes.Attribute{
					attributetypes.NewAttribute("example.attribute",
						s.accountStr,
						attributetypes.AttributeType_String,
						[]byte("example attribute value string"),
						nil,
						"",
					),
				},
				Pagination: &query.PageResponse{NextKey: nil, Total: 1},
			},
		},
		{
			"scan account attribute by suffix but send prefix",
			fmt.Sprintf("%s/provenance/attribute/v1/attribute/%s/scan/%s", baseURL, s.accountAddr, "example"),
			map[string]string{},
			false,
			&attributetypes.QueryScanResponse{},
			&attributetypes.QueryScanResponse{
				Account:    s.accountAddr.String(),
				Attributes: nil,
				Pagination: &query.PageResponse{},
			},
		},
		{
			name:     "get account data that exists",
			url:      fmt.Sprintf("%s/provenance/attribute/v1/accountdata/%s", baseURL, s.accountAddr),
			headers:  map[string]string{},
			expErr:   false,
			respType: &attributetypes.QueryAccountDataResponse{},
			expected: &attributetypes.QueryAccountDataResponse{
				Value: "example accountdata value string",
			},
		},
		{
			name:     "get account data that does not exist",
			url:      fmt.Sprintf("%s/provenance/attribute/v1/accountdata/%s", baseURL, sdk.AccAddress("unknown_address_____").String()),
			headers:  map[string]string{},
			expErr:   false,
			respType: &attributetypes.QueryAccountDataResponse{},
			expected: &attributetypes.QueryAccountDataResponse{
				Value: "",
			},
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			resp, err := sdktestutil.GetRequestWithHeaders(tc.url, tc.headers)
			s.Require().NoError(err)
			err = val.ClientCtx.Codec.UnmarshalJSON(resp, tc.respType)

			if tc.expErr {
				s.Require().Error(err)
			} else {
				s.Require().NoError(err)
				s.Require().Equal(tc.expected.String(), tc.respType.String())
			}
		})
	}
}

func TestIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(IntegrationTestSuite))
}
