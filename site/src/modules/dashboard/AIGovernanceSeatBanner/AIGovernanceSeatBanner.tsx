import { useDashboard } from "modules/dashboard/useDashboard";
import type { FC } from "react";
import { AIGovernanceSeatBannerView } from "./AIGovernanceSeatBannerView";

export const AIGovernanceSeatBanner: FC = () => {
	const { entitlements } = useDashboard();
	const feature = entitlements.features.ai_governance_user_limit;

	if (!feature) {
		return null;
	}

	const { actual, entitlement, limit } = feature;

	if (
		entitlement !== "entitled" ||
		actual === undefined ||
		limit === undefined ||
		limit <= 0 ||
		actual <= limit
	) {
		return null;
	}

	return (
		<AIGovernanceSeatBannerView
			variant="over-limit"
			actual={actual}
			limit={limit}
		/>
	);
};
