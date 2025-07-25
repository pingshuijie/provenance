// Package cmd contains provenance daemon init functionality.
package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	cmtos "github.com/cometbft/cometbft/libs/os"
	cmtrand "github.com/cometbft/cometbft/libs/rand"
	cmttypes "github.com/cometbft/cometbft/types"

	cerrs "cosmossdk.io/errors"
	sdkmath "cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/client/input"
	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	crisistypes "github.com/cosmos/cosmos-sdk/x/crisis/types"
	"github.com/cosmos/cosmos-sdk/x/genutil"
	genutiltypes "github.com/cosmos/cosmos-sdk/x/genutil/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govtypesv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/cosmos/go-bip39"

	provconfig "github.com/provenance-io/provenance/cmd/provenanced/config"
	"github.com/provenance-io/provenance/internal/pioconfig"
	markertypes "github.com/provenance-io/provenance/x/marker/types"
)

const (
	// FlagOverwrite defines a flag to overwrite an existing genesis JSON file.
	FlagOverwrite = "overwrite"
	// FlagRecover defines a flag to initialize the private validator key from a specific seed.
	FlagRecover = "recover"
	// FlagTimeoutCommit is the flag string for providing a consensus.timeout_commit setting.
	FlagTimeoutCommit = "timeout-commit"
)

// InitCmd Creates a command for generating genesis and config files.
func InitCmd(mbm module.BasicManager) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init [moniker]",
		Short: "Initialize files for a provenance daemon node",
		Long:  `Initialize validators's and node's configuration files.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args[0]) == 0 {
				return fmt.Errorf("no moniker provided")
			}
			return Init(cmd, mbm, args[0])
		},
	}
	cmd.Flags().String(flags.FlagChainID, "", "Genesis file chain-id, if left blank will be randomly created")
	cmd.Flags().BoolP(FlagRecover, "r", false, "Interactive key recovery from mnemonic")
	cmd.Flags().BoolP(FlagOverwrite, "o", false, "Overwrite the genesis.json file")
	cmd.Flags().String(provconfig.CustomDenomFlag, "", "Custom denom, optional")
	cmd.Flags().Duration(FlagTimeoutCommit, 0, "The consensus.timeout_commit value to start with (default is 5s for mainnet or testnet, 1s otherwise)")
	return cmd
}

// Init initializes genesis and config files.
func Init(
	cmd *cobra.Command,
	mbm module.BasicManager,
	moniker string,
) error {
	chainID, _ := cmd.Flags().GetString(flags.FlagChainID)
	isTestnet, _ := cmd.Flags().GetBool(provconfig.EnvTypeFlag)
	doRecover, _ := cmd.Flags().GetBool(FlagRecover)
	doOverwrite, _ := cmd.Flags().GetBool(FlagOverwrite)
	timeoutCommit, err := cmd.Flags().GetDuration(FlagTimeoutCommit)
	if err != nil {
		return fmt.Errorf("invalid --%s: %w", FlagTimeoutCommit, err)
	}

	if err = provconfig.EnsureConfigDir(cmd); err != nil {
		return err
	}

	// Get the current configs. This will just be all defaults if the files don't exist.
	appConfig, err := provconfig.ExtractAppConfig(cmd)
	if err != nil {
		return err
	}
	cmtConfig, err := provconfig.ExtractCmtConfig(cmd)
	if err != nil {
		return err
	}
	clientConfig, err := provconfig.ExtractClientConfig(cmd)
	if err != nil {
		return err
	}

	// Stop now if the genesis file already exists and an overwrite wasn't requested.
	genFile := cmtConfig.GenesisFile()
	if !doOverwrite && cmtos.FileExists(genFile) {
		return fmt.Errorf("genesis file already exists: %v", genFile)
	}

	// Set a few things in the configs.
	appConfig.MinGasPrices = pioconfig.GetProvConfig().ProvMinGasPrices

	cmtConfig.Moniker = moniker
	if len(chainID) == 0 {
		chainID = "provenance-chain-" + cmtrand.NewRand().Str(6)
		cmd.Printf("chain id: %s\n", chainID)
	}
	clientConfig.ChainID = chainID

	// If a timeout commit wasn't provided and not on a mainnet or testnet, set the timeout commit to 1s.
	if timeoutCommit == 0 && !strings.Contains(chainID, "mainnet") && !strings.Contains(chainID, "testnet") {
		timeoutCommit = 1 * time.Second
	}
	if timeoutCommit > 0 {
		cmtConfig.Consensus.TimeoutCommit = timeoutCommit
	}

	// Gather the bip39 mnemonic if a recover was requested.
	var mnemonic string
	if doRecover {
		inBuf := bufio.NewReader(cmd.InOrStdin())
		mnemonic, err = input.GetString("Enter your bip39 mnemonic", inBuf)
		if err != nil {
			return err
		}

		if !bip39.IsMnemonicValid(mnemonic) {
			return fmt.Errorf("invalid mnemonic")
		}
	}

	// Create and write the node validator files (node_key.json and priv_validator_key.json).
	nodeID, _, err := genutil.InitializeNodeValidatorFilesFromMnemonic(cmtConfig, mnemonic)
	if err != nil {
		return err
	}
	cmd.Printf("node id: %s\n", nodeID)
	clientConfig.Node = cmtConfig.RPC.ListenAddress

	// Create and write the genenis file.
	if err = createAndExportGenesisFile(cmd, client.GetClientContextFromCmd(cmd).Codec, mbm, isTestnet, chainID, genFile); err != nil {
		return err
	}
	// Save the configs.
	provconfig.SaveConfigs(cmd, appConfig, cmtConfig, clientConfig, true)

	return nil
}

// createAndExportGenesisFile creates and writes the genesis file.
func createAndExportGenesisFile(
	cmd *cobra.Command,
	cdc codec.JSONCodec,
	mbm module.BasicManager,
	isTestnet bool,
	chainID, genFile string,
) error {
	minDeposit := int64(1000000000000)  // 1,000,000,000,000
	downtimeJailDurationStr := "86400s" // 1 day
	maxGas := int64(60000000)           // 60,000,000
	if isTestnet {
		cmd.Printf("Using testnet defaults\n")
		minDeposit = 10000000            // 10,000,000
		downtimeJailDurationStr = "600s" // 10 minutes
	} else {
		cmd.Printf("Using mainnet defaults\n")
	}

	downtimeJailDuration, err := time.ParseDuration(downtimeJailDurationStr)
	if err != nil {
		// This is a panic instead of a return because it should only fail if the strings above are defined incorrectly.
		// i.e. It's not an error that should be handled elsewhere. It means you need to fix your shit.
		panic(err)
	}

	appGenState := mbm.DefaultGenesis(cdc)

	// Note: The extra enclosures in here are just a defensive measure to hopefully help prevent
	// things from one section from being used in another when they're copy/pasted around.

	provCfg := pioconfig.GetProvConfig()

	// Set the mint parameters
	{
		moduleName := minttypes.ModuleName
		var mintGenState minttypes.GenesisState
		cdc.MustUnmarshalJSON(appGenState[moduleName], &mintGenState)
		mintGenState.Minter.Inflation = sdkmath.LegacyZeroDec()
		mintGenState.Minter.AnnualProvisions = sdkmath.LegacyOneDec()
		mintGenState.Params.MintDenom = provCfg.BondDenom
		mintGenState.Params.InflationMax = sdkmath.LegacyZeroDec()
		mintGenState.Params.InflationMin = sdkmath.LegacyZeroDec()
		mintGenState.Params.InflationRateChange = sdkmath.LegacyOneDec()
		mintGenState.Params.GoalBonded = sdkmath.LegacyOneDec()
		mintGenState.Params.BlocksPerYear = 6311520 // (86400 / 5) * 365.25
		appGenState[moduleName] = cdc.MustMarshalJSON(&mintGenState)
	}

	// Set the staking denom
	{
		moduleName := stakingtypes.ModuleName
		var stakeGenState stakingtypes.GenesisState
		cdc.MustUnmarshalJSON(appGenState[moduleName], &stakeGenState)
		stakeGenState.Params.BondDenom = provCfg.BondDenom
		appGenState[moduleName] = cdc.MustMarshalJSON(&stakeGenState)
	}

	// Set the crisis denom
	{
		moduleName := crisistypes.ModuleName
		var crisisGenState crisistypes.GenesisState
		cdc.MustUnmarshalJSON(appGenState[moduleName], &crisisGenState)
		crisisGenState.ConstantFee.Denom = provCfg.FeeDenom
		appGenState[moduleName] = cdc.MustMarshalJSON(&crisisGenState)
	}

	// Set the gov deposit.
	{
		moduleName := govtypes.ModuleName
		var govGenState govtypesv1.GenesisState
		cdc.MustUnmarshalJSON(appGenState[moduleName], &govGenState)
		govGenState.Params.MinDeposit = sdk.NewCoins(sdk.NewInt64Coin(provCfg.BondDenom, minDeposit))
		appGenState[moduleName] = cdc.MustMarshalJSON(&govGenState)
	}

	// Set slashing stuff.
	{
		moduleName := slashingtypes.ModuleName
		var slashingGenState slashingtypes.GenesisState
		cdc.MustUnmarshalJSON(appGenState[moduleName], &slashingGenState)
		slashingGenState.Params.DowntimeJailDuration = downtimeJailDuration
		appGenState[moduleName] = cdc.MustMarshalJSON(&slashingGenState)
	}

	// Set some staking stuff too.
	{
		moduleName := stakingtypes.ModuleName
		var stakingGenState stakingtypes.GenesisState
		cdc.MustUnmarshalJSON(appGenState[moduleName], &stakingGenState)
		stakingGenState.Params.MaxValidators = 100
		appGenState[moduleName] = cdc.MustMarshalJSON(&stakingGenState)
	}

	// Set the marker unrestricted denom regex.
	// This is different from DefaultUnrestrictedDenomRegex.
	// That variable isn't updated because then the default test denom/bond ("stake")
	// doesn't pass and all sorts of tests needs fixing.
	{
		moduleName := markertypes.ModuleName
		var markerGenState markertypes.GenesisState
		cdc.MustUnmarshalJSON(appGenState[moduleName], &markerGenState)
		markerGenState.Params.UnrestrictedDenomRegex = `[a-zA-Z][a-zA-Z0-9\-\.]{7,83}`
		appGenState[moduleName] = cdc.MustMarshalJSON(&markerGenState)
	}

	appState, err := json.MarshalIndent(appGenState, "", "")
	if err != nil {
		return err
	}

	appGen := &genutiltypes.AppGenesis{}
	if _, err = os.Stat(genFile); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else {
		appGen, err = genutiltypes.AppGenesisFromFile(genFile)
		if err != nil {
			return cerrs.Wrap(err, "Failed to read genesis doc from file")
		}
	}

	appGen.ChainID = chainID
	appGen.AppState = appState
	if appGen.Consensus == nil {
		appGen.Consensus = &genutiltypes.ConsensusGenesis{
			Validators: nil,
			Params:     nil,
		}
	}
	appGen.Consensus.Validators = nil
	if appGen.Consensus.Params == nil {
		appGen.Consensus.Params = cmttypes.DefaultConsensusParams()
	}
	appGen.Consensus.Params.Block.MaxGas = maxGas

	if err = genutil.ExportGenesisFile(appGen, genFile); err != nil {
		return cerrs.Wrap(err, "Failed to export gensis file")
	}

	cmd.Printf("Genesis file created: %s\n", genFile)
	return err
}
