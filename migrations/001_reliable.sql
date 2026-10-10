CREATE TABLE `conversation` (
  `id` varchar(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `type` varchar(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `private_user_a` bigint unsigned DEFAULT NULL,
  `private_user_b` bigint unsigned DEFAULT NULL,
  `group_id` bigint unsigned DEFAULT NULL,
  `last_seq` bigint unsigned NOT NULL DEFAULT 0,
  `updated_at` datetime(6) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_conversation_private` (`private_user_a`,`private_user_b`),
  UNIQUE KEY `uq_conversation_group` (`group_id`),
  CONSTRAINT `chk_conversation_identity` CHECK (
    (`type` = 'private' AND `private_user_a` IS NOT NULL AND `private_user_b` IS NOT NULL AND
     `private_user_a` > 0 AND `private_user_b` > 0 AND
     `private_user_a` <= `private_user_b` AND `group_id` IS NULL AND
     `id` = concat('p:',`private_user_a`,':',`private_user_b`)) OR
    (`type` = 'group' AND `private_user_a` IS NULL AND `private_user_b` IS NULL AND
     `group_id` IS NOT NULL AND `group_id` > 0 AND `id` = concat('g:',`group_id`))
  )
) ENGINE=InnoDB DEFAULT CHARSET=ascii COLLATE=ascii_bin;

CREATE TABLE `message` (
  `id` char(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `client_message_id` char(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `sender_id` bigint unsigned NOT NULL,
  `receiver_id` bigint unsigned NOT NULL,
  `type` varchar(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `media` varchar(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `content` varchar(1024) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  `conversation_id` varchar(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `seq` bigint unsigned NOT NULL,
  `client_sent_at` datetime(6) DEFAULT NULL,
  `server_created_at` datetime(6) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_message_conversation_seq` (`conversation_id`,`seq`),
  KEY `idx_message_history` (`conversation_id`,`seq`,`id`),
  CONSTRAINT `fk_message_conversation` FOREIGN KEY (`conversation_id`) REFERENCES `conversation` (`id`),
  CONSTRAINT `chk_message_identity` CHECK (regexp_like(`id`,'^[0-9a-f]{64}$','c') AND
    regexp_like(`client_message_id`,'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$','c')),
  CONSTRAINT `chk_message_fields` CHECK (`sender_id` > 0 AND `receiver_id` > 0 AND `seq` > 0 AND
    `type` IN ('private','group') AND `media` IN ('text','picture','audio') AND char_length(`content`) BETWEEN 1 AND 1024)
) ENGINE=InnoDB DEFAULT CHARSET=ascii COLLATE=ascii_bin;

CREATE TABLE `message_command` (
  `sender_id` bigint unsigned NOT NULL,
  `client_message_id` char(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `command_id` char(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `payload_hash` char(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `state` varchar(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `reason_code` varchar(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `message_id` char(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `conversation_id` varchar(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `created_at` datetime(6) NOT NULL,
  `completed_at` datetime(6) NOT NULL,
  PRIMARY KEY (`sender_id`,`client_message_id`),
  UNIQUE KEY `uq_message_command_id` (`command_id`),
  KEY `idx_message_command_message` (`message_id`),
  CONSTRAINT `chk_command_identity` CHECK (`sender_id` > 0 AND
    regexp_like(`client_message_id`,'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$','c') AND
    regexp_like(`command_id`,'^[0-9a-f]{64}$','c') AND regexp_like(`payload_hash`,'^[0-9a-f]{64}$','c')),
  CONSTRAINT `chk_command_terminal` CHECK (
    (`state` = 'persisted' AND `message_id` IS NOT NULL AND `message_id` = `command_id` AND `conversation_id` IS NOT NULL AND `reason_code` IS NULL) OR
    (`state` = 'rejected' AND `message_id` IS NULL AND `reason_code` IS NOT NULL)
  )
) ENGINE=InnoDB DEFAULT CHARSET=ascii COLLATE=ascii_bin;

CREATE TABLE `message_outbox` (
  `event_id` char(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `event_type` varchar(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `command_id` char(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `conversation_id` varchar(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `message_id` char(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `payload` json NOT NULL,
  `status` varchar(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'pending',
  `available_at` datetime(6) NOT NULL,
  `lease_token` char(36) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `lease_until` datetime(6) DEFAULT NULL,
  `attempts` int unsigned NOT NULL DEFAULT 0,
  `last_error` text CHARACTER SET utf8mb4 COLLATE utf8mb4_bin,
  `created_at` datetime(6) NOT NULL,
  `completed_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`event_id`),
  UNIQUE KEY `uq_outbox_command_type` (`command_id`,`event_type`),
  KEY `idx_outbox_available` (`status`,`available_at`,`created_at`),
  KEY `idx_outbox_lease` (`status`,`lease_until`),
  CONSTRAINT `chk_outbox_identity` CHECK (regexp_like(`event_id`,'^[0-9a-f]{64}$','c') AND
    regexp_like(`command_id`,'^[0-9a-f]{64}$','c') AND `event_type` IN ('message.persisted','command.result')),
  CONSTRAINT `chk_outbox_lease` CHECK (
    (`status` = 'leased' AND `lease_token` IS NOT NULL AND `lease_until` IS NOT NULL AND `completed_at` IS NULL) OR
    (`status` = 'pending' AND `lease_token` IS NULL AND `lease_until` IS NULL AND `completed_at` IS NULL) OR
    (`status` = 'completed' AND `lease_token` IS NULL AND `lease_until` IS NULL AND `completed_at` IS NOT NULL)
  )
) ENGINE=InnoDB DEFAULT CHARSET=ascii COLLATE=ascii_bin;

CREATE TABLE `message_delivery_cursor` (
  `user_id` bigint unsigned NOT NULL,
  `conversation_id` varchar(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `delivered_seq` bigint unsigned NOT NULL DEFAULT 0,
  `updated_at` datetime(6) NOT NULL,
  PRIMARY KEY (`user_id`,`conversation_id`),
  KEY `idx_delivery_conversation` (`conversation_id`),
  CONSTRAINT `fk_delivery_conversation` FOREIGN KEY (`conversation_id`) REFERENCES `conversation` (`id`),
  CONSTRAINT `chk_delivery_user` CHECK (`user_id` > 0)
) ENGINE=InnoDB DEFAULT CHARSET=ascii COLLATE=ascii_bin;

CREATE TABLE `message_quarantine` (
  `transport_id` varchar(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `sender_id` bigint unsigned DEFAULT NULL,
  `client_message_id` char(36) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `command_id` char(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `payload_hash` char(64) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `raw_body` longblob NOT NULL,
  `raw_headers` json DEFAULT NULL,
  `error_code` varchar(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `error_detail` text CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  `state` varchar(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'open',
  `replay_attempt_id` char(36) CHARACTER SET ascii COLLATE ascii_bin DEFAULT NULL,
  `created_at` datetime(6) NOT NULL,
  `resolved_at` datetime(6) DEFAULT NULL,
  PRIMARY KEY (`transport_id`),
  KEY `idx_quarantine_identity` (`sender_id`,`client_message_id`,`payload_hash`,`state`),
  KEY `idx_quarantine_command` (`command_id`),
  CONSTRAINT `chk_quarantine_identity` CHECK (char_length(`transport_id`) BETWEEN 1 AND 128 AND
    (`sender_id` IS NULL OR `sender_id` > 0) AND
    (`command_id` IS NULL OR regexp_like(`command_id`,'^[0-9a-f]{64}$','c')) AND
    (`payload_hash` IS NULL OR regexp_like(`payload_hash`,'^[0-9a-f]{64}$','c'))),
  CONSTRAINT `chk_quarantine_state` CHECK (
    (`state` IN ('open','replay_pending') AND `resolved_at` IS NULL) OR
    (`state` = 'resolved' AND `resolved_at` IS NOT NULL)
  )
) ENGINE=InnoDB DEFAULT CHARSET=ascii COLLATE=ascii_bin;
