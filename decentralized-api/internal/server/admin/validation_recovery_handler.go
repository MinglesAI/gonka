package admin

import (
	"decentralized-api/cosmosclient"
	"decentralized-api/internal/poc"
	"decentralized-api/logging"
	"fmt"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/productscience/inference/api/inference/inference"
	"github.com/productscience/inference/x/inference/types"
)

type ClaimRewardRecoverRequest struct {
	Seed              *int64 `json:"seed,omitempty"`                    // Optional: if not provided, uses stored seed
	ForceClaim        bool   `json:"force_claim"`                       // Force claim even if already claimed
	EpochIndex        uint64 `json:"epoch_index,omitempty"`             // Epoch index to claim rewards for
	ForceValidateAll  bool   `json:"force_validate_all,omitempty"`      // Force validate all missed inferences, ignoring shouldValidate check (only for non-blockchain logic)
	UseBlockchainLogic bool  `json:"use_blockchain_logic,omitempty"`    // Use blockchain logic (getMustBeValidatedInferences) to determine which inferences should be validated. Recommended for accurate results.
	MaxValidations    *int   `json:"max_validations,omitempty"`         // Maximum number of validations to send (default: 1000, max: 10000). Prevents system overload.
}

type ClaimRewardRecoverResponse struct {
	Success           bool   `json:"success"`
	Message           string `json:"message"`
	EpochIndex        uint64 `json:"epoch_index"`
	Seed              int64  `json:"seed"`
	MissedValidations int    `json:"missed_validations"`
	OriginalCount     int    `json:"original_count,omitempty"`     // Original count before truncation
	Truncated         bool   `json:"truncated,omitempty"`          // Whether the list was truncated
	AlreadyClaimed    bool   `json:"already_claimed"`
	ClaimExecuted     bool   `json:"claim_executed"`
}

func (s *Server) postClaimRewardRecover(ctx echo.Context) error {
	var req ClaimRewardRecoverRequest
	if err := ctx.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid request body")
	}

	var epochIndex uint64
	var seedValue int64
	var isUsingPreviousSeed bool

	previousSeed := s.configManager.GetPreviousSeed()

	if req.EpochIndex != 0 {
		epochIndex = req.EpochIndex
		if req.Seed != nil {
			seedValue = *req.Seed
		} else {
			generatedSeed, err := poc.CreateSeedForEpoch(s.recorder, epochIndex)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError,
					"Failed to generate seed for epoch "+strconv.FormatUint(epochIndex, 10)+": "+err.Error())
			}
			seedValue = generatedSeed
			logging.Info("Generated seed for custom epoch", types.Validation,
				"epochIndex", epochIndex, "seed", seedValue)
		}
		isUsingPreviousSeed = false
	} else {
		epochIndex = previousSeed.EpochIndex
		if req.Seed != nil {
			seedValue = *req.Seed
		} else {
			seedValue = previousSeed.Seed
			if seedValue == 0 {
				generatedSeed, err := poc.CreateSeedForEpoch(s.recorder, epochIndex)
				if err != nil {
					return echo.NewHTTPError(http.StatusInternalServerError,
						"Failed to generate seed for epoch "+strconv.FormatUint(epochIndex, 10)+": "+err.Error())
				}
				seedValue = generatedSeed
				logging.Info("Generated seed for previous epoch", types.Validation,
					"epochIndex", epochIndex, "seed", seedValue)
			}
		}
		isUsingPreviousSeed = true
	}

	alreadyClaimed := isUsingPreviousSeed && s.configManager.IsPreviousSeedClaimed()
	if alreadyClaimed && !req.ForceClaim {
		return ctx.JSON(http.StatusOK, ClaimRewardRecoverResponse{
			Success:           false,
			Message:           "Rewards already claimed for this epoch. Use force_claim=true to override.",
			EpochIndex:        epochIndex,
			Seed:              seedValue,
			MissedValidations: 0,
			AlreadyClaimed:    true,
			ClaimExecuted:     false,
		})
	}

	logging.Info("Starting manual validation recovery", types.Validation,
		"epochIndex", epochIndex,
		"seed", seedValue,
		"alreadyClaimed", alreadyClaimed,
		"forceClaim", req.ForceClaim,
		"forceValidateAll", req.ForceValidateAll,
		"useBlockchainLogic", req.UseBlockchainLogic)

	// Detect missed validations
	var missedInferences []types.Inference
	var err error
	if req.UseBlockchainLogic {
		missedInferences, err = s.validator.DetectMissedValidationsUsingBlockchainLogic(epochIndex, seedValue)
	} else {
		missedInferences, err = s.validator.DetectMissedValidations(epochIndex, seedValue, req.ForceValidateAll)
	}
	if err != nil {
		logging.Error("Failed to detect missed validations", types.Validation, "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Failed to detect missed validations: "+err.Error())
	}

	missedCount := len(missedInferences)
	
	// Apply max validations limit
	maxValidations := 1000 // Default limit
	if req.MaxValidations != nil {
		maxValidations = *req.MaxValidations
		if maxValidations < 1 {
			maxValidations = 1
		}
		if maxValidations > 10000 {
			maxValidations = 10000
			logging.Warn("MaxValidations exceeded maximum, using 10000", types.Validation, "requested", *req.MaxValidations)
		}
	}
	
	originalCount := missedCount
	if missedCount > maxValidations {
		missedInferences = missedInferences[:maxValidations]
		missedCount = maxValidations
		logging.Warn("Missed validations list truncated", types.Validation,
			"epochIndex", epochIndex,
			"originalCount", originalCount,
			"truncatedCount", missedCount,
			"maxValidations", maxValidations)
	}
	
	logging.Info("Manual recovery detected missed validations", types.Validation,
		"epochIndex", epochIndex,
		"missedCount", missedCount,
		"originalCount", originalCount,
		"maxValidations", maxValidations)

	// Execute recovery validations
	if missedCount > 0 {
		recoveredCount, _ := s.validator.ExecuteRecoveryValidations(missedInferences)

		logging.Info("Manual recovery validations completed", types.Validation,
			"epochIndex", epochIndex,
			"recoveredCount", recoveredCount,
			"missedCount", missedCount,
		)

		if recoveredCount > 0 {
			s.validator.WaitForValidationsToBeRecorded()
		}
	}

	// Claim rewards if not already claimed or if forced
	claimExecuted := false
	if !alreadyClaimed || req.ForceClaim {
		// Cast to concrete type for RequestMoney
		concreteRecorder := s.recorder.(*cosmosclient.InferenceCosmosClient)
		err := concreteRecorder.ClaimRewards(&inference.MsgClaimRewards{
			Seed:       seedValue,
			EpochIndex: epochIndex,
		})
		if err != nil {
			logging.Error("Failed to claim rewards in manual recovery", types.Claims, "error", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to claim rewards: "+err.Error())
		}

		if isUsingPreviousSeed {
			err = s.configManager.MarkPreviousSeedClaimed()
			if err != nil {
				logging.Error("Failed to mark seed as claimed", types.Claims, "error", err)
			}
		}

		claimExecuted = true
		logging.Info("Manual recovery claim executed", types.Claims, "epochIndex", epochIndex)
	}

	truncated := originalCount > missedCount
	response := ClaimRewardRecoverResponse{
		Success:           true,
		Message:           "Manual claim reward recovery completed successfully",
		EpochIndex:        epochIndex,
		Seed:              seedValue,
		MissedValidations: missedCount,
		AlreadyClaimed:    alreadyClaimed,
		ClaimExecuted:     claimExecuted,
	}
	if truncated {
		response.OriginalCount = originalCount
		response.Truncated = true
		response.Message = fmt.Sprintf("Manual claim reward recovery completed successfully. Found %d missed validations, but only %d were sent due to max_validations limit. You may need to run recovery again to send remaining validations.", originalCount, missedCount)
	}
	
	return ctx.JSON(http.StatusOK, response)
}
