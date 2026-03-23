import Link from "@mui/material/Link";
import { LicenseAIGovernance90PercentWarningText } from "api/typesGenerated";
import { Alert } from "components/Alert/Alert";
import type { FC } from "react";

type AIGovernanceSeatBannerViewProps =
	| { variant: "over-limit"; actual: number; limit: number }
	| { variant: "near-limit" };

export const AIGovernanceSeatBannerView: FC<AIGovernanceSeatBannerViewProps> = (
	props,
) => {
	if (props.variant === "near-limit") {
		return (
			<Alert severity="warning" prominent>
				{LicenseAIGovernance90PercentWarningText}
			</Alert>
		);
	}

	const { actual, limit } = props;
	const overPercent = Math.floor(((actual - limit) / limit) * 100);

	return (
		<Alert severity="warning" prominent>
			Your organization is using {actual} / {limit} AI Governance user seats (
			{overPercent}% over the limit). Contact{" "}
			<Link href="mailto:sales@coder.com">sales@coder.com</Link>
		</Alert>
	);
};
