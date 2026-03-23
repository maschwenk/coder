import { chromatic } from "testHelpers/chromatic";
import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import { AIGovernanceSeatBannerView } from "./AIGovernanceSeatBannerView";

const meta: Meta<typeof AIGovernanceSeatBannerView> = {
	title: "modules/dashboard/AIGovernanceSeatBannerView",
	parameters: { chromatic },
	component: AIGovernanceSeatBannerView,
};

export default meta;
type Story = StoryObj<typeof AIGovernanceSeatBannerView>;

export const OverLimit: Story = {
	args: {
		actual: 110,
		limit: 100,
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByRole("alert")).toHaveTextContent(
			/110 \/ 100 AI Governance user seats \(10% over the limit\)/,
		);
		await expect(
			canvas.getByRole("link", { name: "sales@coder.com" }),
		).toHaveAttribute("href", "mailto:sales@coder.com");
	},
};

export const HighOverage: Story = {
	args: {
		actual: 200,
		limit: 100,
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByRole("alert")).toHaveTextContent(
			/200 \/ 100 AI Governance user seats \(100% over the limit\)/,
		);
	},
};

export const SmallOverage: Story = {
	args: {
		actual: 101,
		limit: 100,
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByRole("alert")).toHaveTextContent(
			/101 \/ 100 AI Governance user seats \(1% over the limit\)/,
		);
	},
};

export const FloorPercentage: Story = {
	args: {
		actual: 106,
		limit: 101,
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByRole("alert")).toHaveTextContent(
			/106 \/ 101 AI Governance user seats \(4% over the limit\)/,
		);
	},
};

export const LargeNumbers: Story = {
	args: {
		actual: 1200,
		limit: 1000,
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByRole("alert")).toHaveTextContent(
			/1200 \/ 1000 AI Governance user seats \(20% over the limit\)/,
		);
	},
};
