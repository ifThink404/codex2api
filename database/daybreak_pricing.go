package database

// Daybreak 专属价格优先于基础模型，同步价不能覆盖管理员的手工价格。
func daybreakAliasPricing(baseModel, alias string) *ModelPricing {
	pricing := *GetModelPricing(baseModel)
	override, ok := lookupModelPricingOverride(alias)
	if !ok {
		return &pricing
	}
	if override.Source != ModelPricingSourceCustom {
		baseKey := PricingManagementModelKey(baseModel)
		canonicalKey := CanonicalBillingModelKey(baseModel)
		if ModelPricingSourceFor(baseKey) == ModelPricingSourceCustom ||
			ModelPricingSourceFor(canonicalKey) == ModelPricingSourceCustom {
			return &pricing
		}
	}
	override.applyNonZero(&pricing)
	return &pricing
}
