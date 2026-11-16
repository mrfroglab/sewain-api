package service

import (
	"context"
	"encoding/json"
)

// AdminMetrics is GET /api/v1/admin/metrics (PRD §9.8, §17), read from the analytics views of the core schema:
// bookings per status with rent value, rent value and the "if commission were active" amount per month, disputes per
// case type, the request funnel (% answered within 24 hours, accepted, completed) and completed rentals per week
// (North Star).
func (s *Service) AdminMetrics(ctx context.Context) (map[string]json.RawMessage, error) {
	queries := map[string]string{
		"bookingsByStatus": `SELECT coalesce(jsonb_agg(jsonb_build_object('status', status, 'bookings', bookings, 'rentValue', rent_value)
			ORDER BY status), '[]') FROM analytics_bookings_by_status`,
		"rentValueByMonth": `SELECT coalesce(jsonb_agg(jsonb_build_object('month', to_char(month, 'YYYY-MM'), 'completed', completed,
			'rentValue', rent_value, 'commissionIfActive', commission_if_active, 'commissionCharged', commission_charged)
			ORDER BY month), '[]') FROM analytics_rent_value`,
		"totals": `SELECT jsonb_build_object(
			'rentValue', coalesce(sum(rent_value), 0), 'commissionIfActive', coalesce(sum(commission_if_active), 0),
			'commissionCharged', coalesce(sum(commission_charged), 0), 'completed', coalesce(sum(completed), 0))
			FROM analytics_rent_value`,
		"disputesByCase": `SELECT coalesce(jsonb_agg(jsonb_build_object('caseType', case_type, 'layer', layer, 'status', status,
			'disputes', disputes) ORDER BY case_type, layer, status), '[]') FROM analytics_disputes_by_case`,
		"requests": `SELECT jsonb_build_object('requests', f.requests, 'accepted', f.accepted, 'completed', f.completed,
			'acceptedPercent', coalesce(f.accepted_percent, 0), 'completedPercent', coalesce(f.completed_percent, 0),
			'avgResponseSeconds', coalesce(extract(epoch FROM f.avg_response_time)::bigint, 0),
			'respondedWithin24hPercent', coalesce(r.pct, 0))
			FROM analytics_request_funnel f, (
			  SELECT round(100.0 * count(*) FILTER (WHERE coalesce(accepted_at, declined_at) - created_at < interval '24 hours')
			         / nullif(count(*), 0), 1) AS pct FROM bookings) r`,
		"completedPerWeek": `SELECT coalesce(jsonb_agg(jsonb_build_object('week', to_char(week, 'YYYY-MM-DD'), 'completed', completed)
			ORDER BY week), '[]') FROM analytics_weekly_completed`,
	}
	out := make(map[string]json.RawMessage, len(queries))
	for key, q := range queries {
		var raw json.RawMessage
		if err := s.pool.QueryRow(ctx, q).Scan(&raw); err != nil {
			return nil, err
		}
		out[key] = raw
	}
	return out, nil
}
