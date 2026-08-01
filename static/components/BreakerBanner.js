function BreakerBanner({
	state,
	openSince,
	openReason,
	pendingReplayCount,
	rapidFailures5m,
	onReset,
}) {
	if (state !== "open") return null;

	const sinceStr = openSince
		? new Date(openSince).toLocaleTimeString()
		: "unknown";

	return (
		<div
			role="alert"
			className="w-full bg-red-600 dark:bg-red-800 text-white px-3 sm:px-6 lg:px-8 py-3 flex flex-wrap items-center justify-between gap-2 shadow-md"
		>
			<div className="flex items-center gap-3 min-w-0">
				<span className="font-bold text-sm sm:text-base whitespace-nowrap">
					⚠ Breaker OPEN
				</span>
				{openReason && (
					<span className="text-red-100 text-xs sm:text-sm truncate">
						{openReason}
					</span>
				)}
				<span className="text-red-200 text-xs whitespace-nowrap">
					since {sinceStr}
				</span>
			</div>

			<div className="flex items-center gap-3">
				<span className="text-red-200 text-xs whitespace-nowrap">
					{pendingReplayCount || 0} replay queued • {rapidFailures5m || 0}{" "}
					rapid-fails/5m
				</span>
				<button
					type="button"
					onClick={onReset}
					className="px-3 py-1.5 rounded-md border border-white/60 text-white text-xs sm:text-sm font-medium hover:bg-white/10 transition-colors focus:outline-none focus:ring-2 focus:ring-white/50"
				>
					Reset
				</button>
			</div>
		</div>
	);
}
window.BreakerBanner = BreakerBanner;
