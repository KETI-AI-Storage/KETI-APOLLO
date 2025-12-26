// ============================================
// APOLLO ForecastService Server
// 노드 자원 사용량 예측 (LSTM/Prophet/ARIMA)
// ============================================

package forecaster

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"apollo/pkg/types"
)

// ForecastService provides resource usage predictions
type ForecastService struct {
	// Historical data store
	historicalData map[string]*NodeMetricHistory // key: node_name
	historyMux     sync.RWMutex

	// Prediction cache
	predictions   map[string]*types.ResourcePrediction // key: node_name
	predictionMux sync.RWMutex

	// Configuration
	config ForecastConfig

	// Prediction model (pluggable)
	predictor Predictor

	// Statistics
	stats ForecastStats
}

// ForecastConfig holds configuration for the forecast service
type ForecastConfig struct {
	ModelType             string        // lstm, prophet, arima, simple
	HistoryWindowMinutes  int           // How much history to keep
	PredictionHorizonMin  int           // How far ahead to predict
	UpdateIntervalSeconds int           // How often to update predictions
	OverloadThreshold     float64       // Threshold for overload warning (0-100)
	MinDataPoints         int           // Minimum data points for prediction
}

// NodeMetricHistory stores historical metrics for a node
type NodeMetricHistory struct {
	NodeName    string
	DataPoints  []MetricDataPoint
	MaxPoints   int
	LastUpdated time.Time
}

// MetricDataPoint represents a single metric observation
type MetricDataPoint struct {
	Timestamp       time.Time
	CPUUtilization  float64
	MemUtilization  float64
	IOUtilization   float64
	GPUUtilization  float64
	NetworkRxBytes  int64
	NetworkTxBytes  int64
}

// ForecastStats tracks forecast service statistics
type ForecastStats struct {
	TotalPredictions     int64
	TotalDataPoints      int64
	PredictionAccuracy   float64 // Rolling average
	LastPredictionTime   time.Time
}

// Predictor interface for pluggable prediction models
type Predictor interface {
	Name() string
	Predict(history []MetricDataPoint, horizonMinutes int) (*types.ResourcePrediction, error)
}

// NewForecastService creates a new ForecastService
func NewForecastService() *ForecastService {
	svc := &ForecastService{
		historicalData: make(map[string]*NodeMetricHistory),
		predictions:    make(map[string]*types.ResourcePrediction),
		config: ForecastConfig{
			ModelType:             "simple", // Default to simple moving average
			HistoryWindowMinutes:  60,
			PredictionHorizonMin:  30,
			UpdateIntervalSeconds: 60,
			OverloadThreshold:     80.0,
			MinDataPoints:         10,
		},
	}

	// Set default predictor
	svc.predictor = &SimpleMovingAveragePredictor{windowSize: 10}

	return svc
}

// SetConfig updates the forecast configuration
func (s *ForecastService) SetConfig(config ForecastConfig) {
	s.config = config

	// Update predictor based on model type
	switch config.ModelType {
	case "simple":
		s.predictor = &SimpleMovingAveragePredictor{windowSize: 10}
	case "exponential":
		s.predictor = &ExponentialSmoothingPredictor{alpha: 0.3}
	// LSTM, Prophet, ARIMA would require external libraries
	default:
		s.predictor = &SimpleMovingAveragePredictor{windowSize: 10}
	}
}

// SetPredictor sets a custom predictor
func (s *ForecastService) SetPredictor(predictor Predictor) {
	s.predictor = predictor
}

// RecordMetrics records current metrics for a node
func (s *ForecastService) RecordMetrics(ctx context.Context, nodeName string, insight *types.ClusterInsight) error {
	if insight == nil || insight.NodeResources == nil {
		return fmt.Errorf("invalid insight data")
	}

	s.stats.TotalDataPoints++

	// Calculate utilization percentages
	cpuUtil := float64(insight.NodeResources.CPUUsedCores) / float64(insight.NodeResources.CPUTotalCores) * 100
	memUtil := float64(insight.NodeResources.MemoryUsedBytes) / float64(insight.NodeResources.MemoryTotalBytes) * 100

	// Calculate average IO utilization
	var ioUtil float64
	if len(insight.StorageDevices) > 0 {
		var total float64
		for _, storage := range insight.StorageDevices {
			total += storage.UtilizationPercent
		}
		ioUtil = total / float64(len(insight.StorageDevices))
	}

	// Calculate GPU utilization if available
	var gpuUtil float64
	if insight.NodeResources.GPUAvailable > 0 {
		gpuUtil = float64(insight.NodeResources.GPUUsed) / float64(insight.NodeResources.GPUAvailable+insight.NodeResources.GPUUsed) * 100
	}

	dataPoint := MetricDataPoint{
		Timestamp:      time.Now(),
		CPUUtilization: cpuUtil,
		MemUtilization: memUtil,
		IOUtilization:  ioUtil,
		GPUUtilization: gpuUtil,
	}

	s.historyMux.Lock()
	defer s.historyMux.Unlock()

	history, exists := s.historicalData[nodeName]
	if !exists {
		maxPoints := s.config.HistoryWindowMinutes * 6 // Assuming 10-second intervals
		history = &NodeMetricHistory{
			NodeName:   nodeName,
			DataPoints: make([]MetricDataPoint, 0, maxPoints),
			MaxPoints:  maxPoints,
		}
		s.historicalData[nodeName] = history
	}

	// Add data point (circular buffer)
	if len(history.DataPoints) >= history.MaxPoints {
		history.DataPoints = history.DataPoints[1:]
	}
	history.DataPoints = append(history.DataPoints, dataPoint)
	history.LastUpdated = time.Now()

	return nil
}

// GetPrediction returns the current prediction for a node
func (s *ForecastService) GetPrediction(nodeName string) *types.ResourcePrediction {
	s.predictionMux.RLock()
	defer s.predictionMux.RUnlock()
	return s.predictions[nodeName]
}

// GeneratePrediction generates a new prediction for a node
func (s *ForecastService) GeneratePrediction(ctx context.Context, nodeName string) (*types.ResourcePrediction, error) {
	s.stats.TotalPredictions++
	s.stats.LastPredictionTime = time.Now()

	s.historyMux.RLock()
	history, exists := s.historicalData[nodeName]
	if !exists || len(history.DataPoints) < s.config.MinDataPoints {
		s.historyMux.RUnlock()
		return nil, fmt.Errorf("insufficient historical data for node %s (have %d, need %d)",
			nodeName, len(history.DataPoints), s.config.MinDataPoints)
	}
	dataPoints := make([]MetricDataPoint, len(history.DataPoints))
	copy(dataPoints, history.DataPoints)
	s.historyMux.RUnlock()

	// Generate prediction using the configured predictor
	prediction, err := s.predictor.Predict(dataPoints, s.config.PredictionHorizonMin)
	if err != nil {
		return nil, fmt.Errorf("prediction failed: %w", err)
	}

	prediction.NodeName = nodeName
	prediction.ModelType = s.predictor.Name()
	prediction.PredictionTime = time.Now()
	prediction.HorizonMinutes = s.config.PredictionHorizonMin

	// Determine if overload is predicted
	prediction.PredictedOverload = prediction.PredictedCPU > s.config.OverloadThreshold ||
		prediction.PredictedMemory > s.config.OverloadThreshold ||
		prediction.PredictedIO > s.config.OverloadThreshold

	// Cache the prediction
	s.predictionMux.Lock()
	s.predictions[nodeName] = prediction
	s.predictionMux.Unlock()

	log.Printf("[ForecastService] Generated prediction for %s: CPU=%.1f%%, Mem=%.1f%%, IO=%.1f%%, Overload=%v",
		nodeName, prediction.PredictedCPU, prediction.PredictedMemory, prediction.PredictedIO, prediction.PredictedOverload)

	return prediction, nil
}

// GenerateAllPredictions generates predictions for all known nodes
func (s *ForecastService) GenerateAllPredictions(ctx context.Context) map[string]*types.ResourcePrediction {
	s.historyMux.RLock()
	nodeNames := make([]string, 0, len(s.historicalData))
	for name := range s.historicalData {
		nodeNames = append(nodeNames, name)
	}
	s.historyMux.RUnlock()

	results := make(map[string]*types.ResourcePrediction)
	for _, nodeName := range nodeNames {
		prediction, err := s.GeneratePrediction(ctx, nodeName)
		if err != nil {
			log.Printf("[ForecastService] Failed to generate prediction for %s: %v", nodeName, err)
			continue
		}
		results[nodeName] = prediction
	}

	return results
}

// GetStats returns service statistics
func (s *ForecastService) GetStats() ForecastStats {
	return s.stats
}

// GetHistoryLength returns the number of data points for a node
func (s *ForecastService) GetHistoryLength(nodeName string) int {
	s.historyMux.RLock()
	defer s.historyMux.RUnlock()

	if history, exists := s.historicalData[nodeName]; exists {
		return len(history.DataPoints)
	}
	return 0
}

// ============================================
// Built-in Predictors
// ============================================

// SimpleMovingAveragePredictor uses simple moving average for prediction
type SimpleMovingAveragePredictor struct {
	windowSize int
}

func (p *SimpleMovingAveragePredictor) Name() string {
	return "simple_moving_average"
}

func (p *SimpleMovingAveragePredictor) Predict(history []MetricDataPoint, horizonMinutes int) (*types.ResourcePrediction, error) {
	if len(history) == 0 {
		return nil, fmt.Errorf("no historical data")
	}

	// Use last N points for moving average
	start := len(history) - p.windowSize
	if start < 0 {
		start = 0
	}
	window := history[start:]

	var cpuSum, memSum, ioSum, gpuSum float64
	for _, dp := range window {
		cpuSum += dp.CPUUtilization
		memSum += dp.MemUtilization
		ioSum += dp.IOUtilization
		gpuSum += dp.GPUUtilization
	}

	n := float64(len(window))

	// Calculate trend (simple linear regression on window)
	cpuTrend := calculateTrend(window, func(dp MetricDataPoint) float64 { return dp.CPUUtilization })
	memTrend := calculateTrend(window, func(dp MetricDataPoint) float64 { return dp.MemUtilization })
	ioTrend := calculateTrend(window, func(dp MetricDataPoint) float64 { return dp.IOUtilization })

	// Project forward using average + trend
	prediction := &types.ResourcePrediction{
		PredictedCPU:    clamp(cpuSum/n+cpuTrend*float64(horizonMinutes), 0, 100),
		PredictedMemory: clamp(memSum/n+memTrend*float64(horizonMinutes), 0, 100),
		PredictedIO:     clamp(ioSum/n+ioTrend*float64(horizonMinutes), 0, 100),
		PredictedGPU:    gpuSum / n, // GPU typically doesn't trend as predictably
		Confidence:      0.7,        // Fixed confidence for simple model
	}

	return prediction, nil
}

// ExponentialSmoothingPredictor uses exponential smoothing for prediction
type ExponentialSmoothingPredictor struct {
	alpha float64 // Smoothing factor (0-1)
}

func (p *ExponentialSmoothingPredictor) Name() string {
	return "exponential_smoothing"
}

func (p *ExponentialSmoothingPredictor) Predict(history []MetricDataPoint, horizonMinutes int) (*types.ResourcePrediction, error) {
	if len(history) == 0 {
		return nil, fmt.Errorf("no historical data")
	}

	// Exponential smoothing
	var cpuSmooth, memSmooth, ioSmooth, gpuSmooth float64
	cpuSmooth = history[0].CPUUtilization
	memSmooth = history[0].MemUtilization
	ioSmooth = history[0].IOUtilization
	gpuSmooth = history[0].GPUUtilization

	for i := 1; i < len(history); i++ {
		cpuSmooth = p.alpha*history[i].CPUUtilization + (1-p.alpha)*cpuSmooth
		memSmooth = p.alpha*history[i].MemUtilization + (1-p.alpha)*memSmooth
		ioSmooth = p.alpha*history[i].IOUtilization + (1-p.alpha)*ioSmooth
		gpuSmooth = p.alpha*history[i].GPUUtilization + (1-p.alpha)*gpuSmooth
	}

	prediction := &types.ResourcePrediction{
		PredictedCPU:    clamp(cpuSmooth, 0, 100),
		PredictedMemory: clamp(memSmooth, 0, 100),
		PredictedIO:     clamp(ioSmooth, 0, 100),
		PredictedGPU:    clamp(gpuSmooth, 0, 100),
		Confidence:      0.75,
	}

	return prediction, nil
}

// Helper functions

func calculateTrend(data []MetricDataPoint, getValue func(MetricDataPoint) float64) float64 {
	if len(data) < 2 {
		return 0
	}

	// Simple linear regression
	n := float64(len(data))
	var sumX, sumY, sumXY, sumXX float64

	for i, dp := range data {
		x := float64(i)
		y := getValue(dp)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}

	denominator := n*sumXX - sumX*sumX
	if math.Abs(denominator) < 0.0001 {
		return 0
	}

	slope := (n*sumXY - sumX*sumY) / denominator
	return slope
}

func clamp(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
