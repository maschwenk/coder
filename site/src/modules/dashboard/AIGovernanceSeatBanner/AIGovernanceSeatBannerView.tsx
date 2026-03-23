import Link from "@mui/material/Link";
import { Alert } from "components/Alert/Alert";
import type { FC } from "react";

interface AIGovernanceSeatBannerViewProps {
	actual: number;
	limit: number;
}

export const AIGovernanceSeatBannerView: FC<
	AIGovernanceSeatBannerViewProps
> = ({ actual, limit }) => {
	const overPercent = Math.floor(((actual - limit) / limit) * 100);

	return (
		<Alert severity="warning" prominent>
			Your organization is using {actual} / {limit} AI Governance user seats (
			{overPercent}% over the limit). Contact{" "}
			<Link href="mailto:sales@coder.com">sales@coder.com</Link>
		</Alert>
	);
};
