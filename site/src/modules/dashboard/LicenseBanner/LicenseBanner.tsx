import {
	LicenseAIGovernance90PercentWarningText,
	LicenseAIGovernanceOverLimitWarningText,
} from "api/typesGenerated";
import { useDashboard } from "modules/dashboard/useDashboard";
import type { FC } from "react";
import { LicenseBannerView } from "./LicenseBannerView";

// AI governance seat warnings are rendered by the dedicated
// AIGovernanceSeatBanner component. Filter them from the generic
// LicenseBanner to avoid showing duplicate messages to admins.
const aiGovernanceOverLimitWarningPrefix =
	LicenseAIGovernanceOverLimitWarningText.split("%d")[0];

const isAIGovernanceWarning = (message: string): boolean =>
	message === LicenseAIGovernance90PercentWarningText ||
	message.startsWith(aiGovernanceOverLimitWarningPrefix);

export const LicenseBanner: FC = () => {
	const { entitlements } = useDashboard();
	const { errors } = entitlements;
	const warnings = entitlements.warnings.filter(
		(w) => !isAIGovernanceWarning(w),
	);

	if (errors.length === 0 && warnings.length === 0) {
		return null;
	}

	return <LicenseBannerView errors={errors} warnings={warnings} />;
};
