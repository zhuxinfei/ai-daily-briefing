-- v1.0.1: add trends_diagram_detail column (missing from 003)
ALTER TABLE weekly_issues ADD COLUMN trends_diagram_detail TEXT;
